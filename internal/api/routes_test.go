package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/httpapi"
)

func concretePath(pattern string) string { return strings.ReplaceAll(pattern, "{id}", jobIDA) }

func TestRoutes_TheTableIsTheWholeSurfaceAndEveryRouteSitsBehindAuthentication(t *testing.T) {
	r := newRig(t)
	if len(routes) != 13 || len(Routes()) != 13 {
		t.Fatalf("the route table has %d routes, the documented surface is 13: a route added or removed is a decision (Docs/jobs-api.md)", len(routes))
	}
	for _, rt := range routes {
		path := concretePath(rt.pattern)
		r.store.reset()
		// without a token: refused, and the handler (so the store) never reached
		resp, body := r.do(req{method: rt.method, path: path})
		r.wantError(resp, body, http.StatusUnauthorized, "unauthenticated")
		if resp.Header.Get("WWW-Authenticate") != "Bearer" {
			t.Errorf("%s %s: WWW-Authenticate = %q", rt.method, rt.pattern, resp.Header.Get("WWW-Authenticate"))
		}
		if n := len(r.store.seen()); n != 0 {
			t.Fatalf("%s %s: the store was called %d time(s) for a request with no token", rt.method, rt.pattern, n)
		}
		// the control: with a legal token the very same route is reached (whatever the answer is, it is not the refusal of the login, the path or the verb)
		resp, _ = r.do(req{method: rt.method, path: path, token: r.token(loginA)})
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
			t.Errorf("%s %s with a legal token: %d", rt.method, rt.pattern, resp.StatusCode)
		}
	}
}

func TestRoutes_AWrongVerbOrPathIsTheEnvelopeOf404And405(t *testing.T) {
	r := newRig(t)
	allowed := map[string][]string{}
	for _, rt := range routes {
		allowed[rt.pattern] = append(allowed[rt.pattern], rt.method)
	}
	for pattern, methods := range allowed {
		path := concretePath(pattern)
		for _, wrong := range []string{http.MethodPut, http.MethodPatch, http.MethodDelete} {
			resp, body := r.do(req{method: wrong, path: path, token: r.token(loginA)})
			e := r.wantError(resp, body, http.StatusMethodNotAllowed, "method_not_allowed")
			if e.Message != "This method is not allowed on this path." {
				t.Errorf("%s %s: message %q is not the platform's", wrong, pattern, e.Message)
			}
			got := strings.Split(strings.ReplaceAll(resp.Header.Get("Allow"), " ", ""), ",")
			for _, m := range methods {
				if !contains(got, m) {
					t.Errorf("%s %s: Allow %q does not name %s", wrong, pattern, resp.Header.Get("Allow"), m)
				}
			}
		}
	}
	// a path nobody serves
	for _, p := range []string{"/v1/job", "/v1/jobs/" + jobIDA + "/nothing", "/v2/jobs", "/v1/jobs/" + jobIDA + "/results.json", "/v1/jobs/" + jobIDA + "/preview/x"} {
		resp, body := r.do(req{method: http.MethodGet, path: p, token: r.token(loginA)})
		e := r.wantError(resp, body, http.StatusNotFound, "not_found")
		if e.Message != "The requested resource was not found." {
			t.Errorf("GET %s: message %q is not the platform's", p, e.Message)
		}
	}
	if n := len(r.store.seen()); n != 0 {
		t.Fatalf("the store was called %d time(s) by requests no route serves", n)
	}
	// the control: the right verb on a served path is served
	if resp, _ := r.get(loginA, "/v1/jobs"); resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/jobs = %d", resp.StatusCode)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// With nothing mounted (the service started with no key source) every job route is the 404 envelope the catch-all has always given: the API is not there, and the probes still are.
func TestRoutes_NothingMountedMeansTheCatchAllAnswers(t *testing.T) {
	h := httpapi.NewHandler(httpapi.Deps{Pinger: pingOK{}, ReadyTimeout: time.Second})
	r := &rig{t: t, srv: httpServer(t, h)}
	for _, rt := range routes {
		resp, body := r.do(req{method: rt.method, path: concretePath(rt.pattern), token: "anything"})
		r.wantError(resp, body, http.StatusNotFound, "not_found")
	}
	if resp, _ := r.do(req{method: http.MethodGet, path: "/healthz"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz = %d", resp.StatusCode)
	}
	// the control: mounted, the same request is not a 404
	m := newRig(t)
	if resp, _ := m.get(loginA, "/v1/jobs"); resp.StatusCode != http.StatusOK {
		t.Fatalf("with the API mounted GET /v1/jobs = %d", resp.StatusCode)
	}
}

func TestRoutes_EachVerbAndPathIsListedOnce(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range Routes() {
		if seen[s] {
			t.Errorf("route %q is listed twice", s)
		}
		seen[s] = true
		if !strings.HasPrefix(s, "GET /v1/jobs") && !strings.HasPrefix(s, "POST /v1/jobs") {
			t.Errorf("route %q is outside /v1/jobs", s)
		}
	}
	if len(seen) != 13 {
		t.Errorf("%d distinct routes, want 13", len(seen))
	}
}
