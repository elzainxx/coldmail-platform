package httpapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/logging"
)

// Deps.Web (Phase 6 A2): the catch-all offers the built web app the GET and HEAD of every path that is not the platform's own, and nothing else. The property that matters is the one that cannot be
// seen from the app's side: with the app on, every answer for an API path is the answer it was with the app off, byte for byte (the request id aside). Every test pairs a handler with a web app and
// one without, and a stub that would answer EVERYTHING it is asked, so that "the API stayed the API" is the catch-all's doing and not the app's good manners.

// stubWeb answers GET and HEAD of any path it is asked with 200 and "WEB:<path>", and records every path it was asked. refuse makes it decline the paths it names (returning false after setting a header,
// as the real app does).
type stubWeb struct {
	mu     sync.Mutex
	asked  []string
	refuse map[string]bool
}

func (s *stubWeb) Serve(w http.ResponseWriter, r *http.Request) bool {
	s.mu.Lock()
	s.asked = append(s.asked, r.Method+" "+r.URL.Path)
	s.mu.Unlock()
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	w.Header().Set("X-Web", "seen")
	if s.refuse[r.URL.Path] {
		return false
	}
	_, _ = fmt.Fprintf(w, "WEB:%s", r.URL.Path)
	return true
}

func (s *stubWeb) askedPaths() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.asked...)
}

// routesOfTheAPI stands in for the routes another package mounts: a POST-only route (a GET of it is the 405 envelope with an Allow header), a GET route, and the public unsubscribe shape.
func routesOfTheAPI(m *http.ServeMux) {
	m.HandleFunc("POST /v1/only-post", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	m.HandleFunc("GET /v1/thing", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"ok": "yes"})
	})
	m.HandleFunc("GET /u/{token}", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("unsubscribe page")) })
}

func handlerWith(t *testing.T, web WebHandler) (http.Handler, *syncBuffer) {
	t.Helper()
	logs := &syncBuffer{}
	log, err := logging.New("debug", logs)
	if err != nil {
		t.Fatal(err)
	}
	return NewHandler(Deps{Logger: log, Pinger: pingerFunc(healthy), ReadyTimeout: time.Second, Now: func() time.Time { return fixedNow }, Mount: routesOfTheAPI, Web: web}), logs
}

var ridInBody = regexp.MustCompile(`req_[0-9a-f]{32}`)

// snapshot is everything a client can see of an answer, with the request id blanked: the status, every header but X-Request-Id (sorted) and the body.
func snapshot(rec *httptest.ResponseRecorder) string {
	var hs []string
	for k, vs := range rec.Header() {
		if k == RequestIDHeader {
			continue
		}
		hs = append(hs, k+": "+strings.Join(vs, ", "))
	}
	sort.Strings(hs)
	return fmt.Sprintf("%d\n%s\n\n%s", rec.Code, strings.Join(hs, "\n"), ridInBody.ReplaceAllString(rec.Body.String(), "req_X"))
}

func serve(h http.Handler, method, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

func TestIsPlatformPath_ListsExactlyThePlatformsOwnPaths(t *testing.T) {
	for _, p := range []string{"/v1", "/v1/", "/v1/jobs", "/v1/anything/else/at/all", "/v2/jobs", "/v10/x", "/v99", "/u", "/u/", "/u/token", "/healthz", "/healthz/", "/healthz/x", "/readyz", "/readyz/x"} {
		if !IsPlatformPath(p) {
			t.Errorf("%q is the platform's own path and IsPlatformPath says it is not", p)
		}
	}
	// the twins: each differs from a platform path in one thing, and is the web app's to answer
	for _, p := range []string{"/", "/jobs", "/jobs/v1", "/v", "/vx", "/v1abc", "/v1.2", "/V1/jobs", "/U/x", "/uploads", "/u-x", "/healthzz", "/ready", "/me/v1", "/assets/app.js", "/config.json", "/index.html", ""} {
		if IsPlatformPath(p) {
			t.Errorf("%q is not the platform's path and IsPlatformPath says it is", p)
		}
	}
}

func TestWeb_EveryAnswerForAPlatformPathIsByteIdenticalWithTheAppOnOrOff(t *testing.T) {
	stub := &stubWeb{}
	off, _ := handlerWith(t, nil)
	on, _ := handlerWith(t, stub)

	methods := []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions}
	paths := []string{
		"/v1", "/v1/", "/v1/nothing-here", "/v1/nothing/here/either", "/v1/only-post", "/v1/thing", "/v1/thing/extra", "/v1/thing?q=1",
		"/v2/jobs", "/v10/anything",
		"/u", "/u/", "/u/abc", "/u/abc/def",
		"/healthz", "/healthz/x", "/readyz", "/readyz/x",
	}
	compared := 0
	for _, p := range paths {
		for _, m := range methods {
			a, b := snapshot(serve(off, m, p)), snapshot(serve(on, m, p))
			if a != b {
				t.Errorf("%s %s differs with the app on:\n--- off\n%s\n--- on\n%s", m, p, a, b)
			}
			compared++
		}
	}
	if compared != len(methods)*len(paths) || compared < 100 {
		t.Fatalf("compared %d answers: the instrument matched too little", compared)
	}
	if asked := stub.askedPaths(); len(asked) != 0 {
		t.Fatalf("the app was offered platform paths: %v", asked)
	}

	// the shapes the comparison stands on are the real ones, so that "identical" is not "identically broken"
	if rec := serve(on, http.MethodGet, "/v1/nothing-here"); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `"code":"not_found"`) {
		t.Fatalf("GET /v1/nothing-here with the app on = %d %s, want the 404 envelope", rec.Code, rec.Body)
	}
	if rec := serve(on, http.MethodGet, "/v1/only-post"); rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "POST" {
		t.Fatalf("GET /v1/only-post with the app on = %d (Allow %q), want 405 with Allow: POST", rec.Code, rec.Header().Get("Allow"))
	}
	if rec := serve(on, http.MethodPost, "/v1/only-post"); rec.Code != http.StatusNoContent {
		t.Fatalf("POST /v1/only-post with the app on = %d, want the route's 204", rec.Code)
	}
	if rec := serve(on, http.MethodGet, "/u/abc"); rec.Code != http.StatusOK || rec.Body.String() != "unsubscribe page" {
		t.Fatalf("GET /u/abc with the app on = %d %q, want the public route's page", rec.Code, rec.Body)
	}
	if rec := serve(on, http.MethodGet, "/healthz"); rec.Code != http.StatusOK {
		t.Fatalf("/healthz with the app on = %d", rec.Code)
	}
	if got := stub.askedPaths(); len(got) != 0 {
		t.Fatalf("the app was offered platform paths while the shapes were checked: %v", got)
	}

	// the control that makes the property mean something: the very same handler DOES offer the app a path that is not the platform's, and the app's answer is what comes back, while the handler
	// without an app answers the 404 envelope it always did
	for _, p := range []string{"/", "/jobs", "/jobs/7/results", "/assets/app.js", "/config.json"} {
		rec := serve(on, http.MethodGet, p)
		if rec.Code != http.StatusOK || rec.Body.String() != "WEB:"+p || rec.Header().Get("X-Web") != "seen" {
			t.Errorf("GET %s with the app on = %d %q, want the app's answer", p, rec.Code, rec.Body)
		}
		if rec := serve(off, http.MethodGet, p); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `"code":"not_found"`) {
			t.Errorf("GET %s with no app = %d %s, want the 404 envelope", p, rec.Code, rec.Body)
		}
	}
}

func TestWeb_WhatTheAppDoesNotServeFallsThroughToTheUnchangedAnswer(t *testing.T) {
	stub := &stubWeb{refuse: map[string]bool{"/assets/missing.js": true}}
	off, _ := handlerWith(t, nil)
	on, _ := handlerWith(t, stub)

	// a path the app declines: the 404 envelope the handler always gave (the app's headers stay, which is how a 404 for a missing asset carries the policy), with the request id in body and header
	rec := serve(on, http.MethodGet, "/assets/missing.js")
	ref := serve(off, http.MethodGet, "/assets/missing.js")
	if rec.Code != http.StatusNotFound || rec.Body.String() == "" || ridInBody.ReplaceAllString(rec.Body.String(), "X") != ridInBody.ReplaceAllString(ref.Body.String(), "X") {
		t.Fatalf("a declined path answered %d %s, want the 404 envelope of the handler with no app (%s)", rec.Code, rec.Body, ref.Body)
	}
	if rec.Header().Get("X-Web") != "seen" {
		t.Fatalf("the headers the app set before it declined were dropped: %v", rec.Header())
	}
	if rid := rec.Header().Get(RequestIDHeader); !requestIDShape.MatchString(rid) || !strings.Contains(rec.Body.String(), rid) {
		t.Fatalf("the declined path's envelope does not carry the request id (%q): %s", rid, rec.Body)
	}

	// a verb the app declines (a POST): the same answer as with no app at all, for a path that is the app's and for one that is nobody's
	for _, p := range []string{"/", "/jobs", "/anything.css"} {
		for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
			a, b := serve(off, m, p), serve(on, m, p)
			if a.Code != http.StatusNotFound || a.Code != b.Code || ridInBody.ReplaceAllString(a.Body.String(), "X") != ridInBody.ReplaceAllString(b.Body.String(), "X") {
				t.Errorf("%s %s: with no app %d %s, with the app %d %s", m, p, a.Code, a.Body, b.Code, b.Body)
			}
		}
	}
	if got := stub.askedPaths(); len(got) != 1+3*3 {
		t.Fatalf("the app was asked %d times (%v), want 10: the catch-all offers it every non-platform request and lets it decline", len(got), got)
	}
}

func TestWeb_TheMiddlewareStillWrapsTheAppsAnswers(t *testing.T) {
	on, logs := handlerWith(t, &stubWeb{})
	rec := serve(on, http.MethodGet, "/jobs/7?q=QUERY-CANARY-5530")
	rid := rec.Header().Get(RequestIDHeader)
	if rec.Code != http.StatusOK || !requestIDShape.MatchString(rid) {
		t.Fatalf("an app answer is %d with request id %q, want 200 and a request id", rec.Code, rid)
	}
	var logged bool
	for _, l := range logs.lines(t) {
		if l["msg"] == "http_request" && l["path"] == "/jobs/7" && l["status"] == float64(200) && l[logging.RequestIDKey] == rid {
			logged = true
		}
	}
	if !logged {
		t.Fatalf("no access log line for the app's answer under its request id: %s", logs.String())
	}
	if strings.Contains(logs.String(), "QUERY-CANARY-5530") {
		t.Fatalf("the query string reached the log: %s", logs.String())
	}
	// HEAD is offered too, and the answer has no body
	if rec := serve(on, http.MethodHead, "/jobs/7"); rec.Code != http.StatusOK {
		t.Fatalf("HEAD /jobs/7 = %d", rec.Code)
	}
}
