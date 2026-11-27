package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

// The public unsubscribe routes carry a per-recipient token in the PATH (/u/{token}, internal/unsubscribe/endpoint). The access log records the path of every request, and a log is read by more people
// than a link is sent to, so everything after /u/ is replaced by [token] whether or not a route took the request. The control in each test is a path that is NOT under a secret prefix, which is logged
// in full: the redaction is the prefix's doing and the log does record paths.

func TestAccessLog_APathUnderASecretPrefixIsLoggedWithoutItsRemainder(t *testing.T) {
	const canary = "TOKEN-CANARY-7714"
	e := mounted(t, func(m *http.ServeMux) {
		m.HandleFunc("/u/{token...}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
		m.HandleFunc("GET /v1/thing/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	})
	for _, p := range []string{"/u/" + canary, "/u/" + canary + "/more", "/u/", "/u/%41" + canary} {
		if rec := e.do(http.MethodGet, p+"?q="+canary); rec.Code != http.StatusNoContent {
			t.Fatalf("GET %s = %d, want the mounted route's 204", p, rec.Code)
		}
	}
	// a request no route takes is the catch-all's 404 and is redacted just the same (an unmounted service still must not log a token)
	bare := mounted(t, nil)
	if rec := bare.do(http.MethodGet, "/u/"+canary); rec.Code != http.StatusNotFound {
		t.Fatalf("an unmounted /u/ = %d, want 404", rec.Code)
	}
	for name, logs := range map[string]string{"mounted": e.logs.String(), "unmounted": bare.logs.String()} {
		if strings.Contains(logs, canary) {
			t.Fatalf("%s: the access log holds the token:\n%s", name, logs)
		}
		if !strings.Contains(logs, `"path":"/u/[token]"`) {
			t.Fatalf("%s: the access log does not record the redacted path:\n%s", name, logs)
		}
	}
	// the control: a path that is not under the prefix is logged in full, and so is the verb
	if rec := e.do(http.MethodGet, "/v1/thing/ID-CANARY-3302"); rec.Code != http.StatusNoContent {
		t.Fatalf("control request = %d", rec.Code)
	}
	if !strings.Contains(e.logs.String(), `"path":"/v1/thing/ID-CANARY-3302"`) {
		t.Fatalf("control failed: the access log does not record an ordinary path in full:\n%s", e.logs.String())
	}
}

func TestLoggedPath_OnlyTheRemainderAfterASecretPrefixIsReplaced(t *testing.T) {
	for in, want := range map[string]string{
		"/u/abc":        "/u/[token]",
		"/u/":           "/u/[token]",
		"/u/a/b/c":      "/u/[token]",
		"/healthz":      "/healthz",
		"/u":            "/u",
		"/user/abc":     "/user/abc",
		"/v1/u/abc":     "/v1/u/abc",
		"/U/abc":        "/U/abc",
		"":              "",
		"/v1/jobs/x/u/": "/v1/jobs/x/u/",
	} {
		if got := loggedPath(in); got != want {
			t.Errorf("loggedPath(%q) = %q, want %q", in, got, want)
		}
	}
	if len(secretPathPrefixes) != 1 || secretPathPrefixes[0] != "/u/" {
		t.Fatalf("the secret prefixes are %v: a new one needs its own cases here", secretPathPrefixes)
	}
}
