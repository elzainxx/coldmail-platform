package modclient

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/config"
)

func TestNew_RefusesBadArgumentsWithoutEchoingThem(t *testing.T) {
	tok := mustToken(t)
	const host = "host-XYZ123.test"
	cases := []struct {
		name   string
		module Module
		url    string
		token  string
		opts   Options
	}{
		{"unknown module", "m9", "http://" + host + ":8080", tok, Options{}},
		{"empty module", "", "http://" + host + ":8080", tok, Options{}},
		{"scheme ftp", M2, "ftp://" + host + ":21", tok, Options{}},
		{"no scheme", M2, host + ":8080", tok, Options{}},
		{"no host", M2, "http:///path", tok, Options{}},
		{"credentials in the URL", M2, "http://user-XYZ123:pw-XYZ123@" + host + ":8080", tok, Options{}},
		{"a user name in the URL", M2, "http://user-XYZ123@" + host + ":8080", tok, Options{}},
		{"a query in the URL", M2, "http://" + host + ":8080?key=1", tok, Options{}},
		{"an empty query in the URL", M2, "http://" + host + ":8080?", tok, Options{}},
		{"a fragment in the URL", M2, "http://" + host + ":8080#x", tok, Options{}},
		{"an opaque URL", M2, "mailto:ops@" + host, tok, Options{}},
		{"an empty secret", M2, "http://" + host, "", Options{}},
		{"a secret with a trailing newline", M2, "http://" + host, tok + "\n", Options{}},
		{"a secret with a space", M2, "http://" + host, tok + " x", Options{}},
		{"a secret with a non-ASCII character", M2, "http://" + host, tok + "é", Options{}},
		{"a negative timeout", M2, "http://" + host, tok, Options{Timeout: -time.Second}},
		{"a timeout over the maximum", M2, "http://" + host, tok, Options{Timeout: MaxRequestTimeout + time.Second}},
		{"negative retries", M2, "http://" + host, tok, Options{Retries: -1}},
		{"too many retries", M2, "http://" + host, tok, Options{Retries: maxRetries + 1}},
		{"a negative backoff", M2, "http://" + host, tok, Options{Backoff: -time.Millisecond}},
		{"a backoff over the cap", M2, "http://" + host, tok, Options{Backoff: MaxBackoff + time.Millisecond}},
		{"a negative body limit", M2, "http://" + host, tok, Options{MaxBodyBytes: -1}},
	}
	if len(cases) < 20 {
		t.Fatal("the table shrank")
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := New(tc.module, tc.url, config.Secret(tc.token), nil, tc.opts)
			if c != nil {
				t.Fatalf("New returned a client with %v", err)
			}
			e := wantKind(t, err, Invalid)
			text := fmt.Sprintf("%v %+v %#v %s", err, err, err, e.Message)
			if leaked := containsAny(text, tok, "XYZ123", "user-", "pw-", "key=1"); leaked != "" {
				t.Fatalf("the error echoes %q: %s", leaked, text)
			}
		})
	}
	// The paired control: the same call with sound arguments works, and the base URL is normalised (trailing slash trimmed, path prefix kept).
	c, err := New(M2, "http://"+host+":8080/prefix/", config.Secret(tok), nil, Options{})
	if err != nil || c.base != "http://"+host+":8080/prefix" || c.Module() != M2 {
		t.Fatalf("control failed: %v, %+v", err, c)
	}
	if c.opts.Timeout != DefaultTimeout || c.opts.MaxBodyBytes != DefaultMaxBodyBytes || c.opts.Retries != 0 {
		t.Fatalf("a zero Options was not completed with the defaults: %+v", c.opts)
	}
}

func TestNew_CopiesTheInjectedClientAndNeverUsesTheDefaultClient(t *testing.T) {
	tok := config.Secret(mustToken(t))
	callerRedirect := 0
	own := &http.Client{Timeout: 7 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { callerRedirect++; return nil }}
	c, err := New(M2, "http://host.test:8080", tok, own, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if c.hc == own || c.hc == http.DefaultClient {
		t.Fatal("the client uses the caller's client (or http.DefaultClient) itself, not a copy")
	}
	if c.hc.Timeout != 7*time.Second {
		t.Errorf("the copy lost the caller's settings: timeout %s", c.hc.Timeout)
	}
	// The caller's client is untouched: its own redirect policy still runs.
	if err := own.CheckRedirect(nil, nil); err != nil || callerRedirect != 1 {
		t.Fatalf("the caller's client was modified: %v, %d", err, callerRedirect)
	}
	// The copy follows no redirect.
	if err := c.hc.CheckRedirect(nil, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("the copy's redirect policy is %v, want http.ErrUseLastResponse", err)
	}

	// A nil client becomes a fresh one with its own transport and no proxy.
	d, err := New(M2, "http://host.test:8080", tok, nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	tr, ok := d.hc.Transport.(*http.Transport)
	if d.hc == http.DefaultClient || !ok || tr == http.DefaultTransport || tr.Proxy != nil {
		t.Fatalf("a nil client did not become a fresh client without a proxy: %+v", d.hc)
	}
	// The paired control: the default transport this one was cloned from DOES have a proxy function, so "no proxy" above is a decision and not an accident.
	if http.DefaultTransport.(*http.Transport).Proxy == nil {
		t.Fatal("control failed: the default transport has no proxy function, so the check above proves nothing")
	}
}

func TestDo_SendsTheCredentialAndTheRequestAsDescribed(t *testing.T) {
	tok := mustToken(t)
	s := newStub(t, func(w http.ResponseWriter, r *http.Request, n int) { jsonReply(w, 200, `{"ok":true}`) })
	c := s.client(t, M2, tok, Options{})

	resp, err := c.Do(ctxT(t, 5*time.Second), Request{
		Module: M2, Route: "m2.run", Method: http.MethodPost, Path: "/v1/run", Expect: []int{200, 202},
		Body: map[string]string{"job_item_id": "ji-1", "user_id": "u-1"},
	})
	if err != nil || resp == nil || resp.Status != 200 || resp.Attempts != 1 {
		t.Fatalf("Do = %+v, %v", resp, err)
	}
	r := s.requests()[0]
	if r.method != "POST" || r.path != "/v1/run" || r.auth != "Bearer "+tok || r.contentType != "application/json" || r.accept != "application/json" || r.userAgent != "platform-modclient" {
		t.Fatalf("request = %+v", r)
	}
	var sent map[string]string
	if err := json.Unmarshal(r.body, &sent); err != nil || sent["job_item_id"] != "ji-1" || sent["user_id"] != "u-1" {
		t.Fatalf("body %q: %v", r.body, err)
	}

	// A query is encoded, escaped and sorted by key; a GET has no body and no content type.
	_, err = c.Do(ctxT(t, 5*time.Second), Request{Route: "m2.report", Method: http.MethodGet, Path: "/v1/report", Query: url.Values{"user_id": {"u-1"}, "campaign_id": {"c-1"}, "cursor": {"a b|c"}}})
	if err != nil {
		t.Fatal(err)
	}
	if r := s.requests()[1]; r.rawQuery != "campaign_id=c-1&cursor=a+b%7Cc&user_id=u-1" || r.contentType != "" || len(r.body) != 0 {
		t.Fatalf("GET = %+v", r)
	}

	// Anonymous sends no credential at all; the paired control is the same request with it.
	if _, err := c.Do(ctxT(t, 5*time.Second), Request{Route: "m2.healthz", Method: http.MethodGet, Path: "/healthz", Anonymous: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Do(ctxT(t, 5*time.Second), Request{Route: "m2.healthz", Method: http.MethodGet, Path: "/healthz"}); err != nil {
		t.Fatal(err)
	}
	if a, b := s.requests()[2].auth, s.requests()[3].auth; a != "" || b != "Bearer "+tok {
		t.Fatalf("anonymous request carried %q, the authenticated one %q", a, b)
	}

	// A path prefix in the base URL is kept.
	p, err := New(M2, s.srv.URL+"/module-two/", config.Secret(tok), transport(t), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Do(ctxT(t, 5*time.Second), getReq("m2.report", "/v1/report")); err != nil {
		t.Fatal(err)
	}
	if got := s.requests()[4].path; got != "/module-two/v1/report" {
		t.Fatalf("path %q", got)
	}
}

func TestDo_RefusesBadRequestsBeforeAnyNetworkCall(t *testing.T) {
	tok := mustToken(t)
	s := newStub(t, func(w http.ResponseWriter, r *http.Request, n int) { jsonReply(w, 200, `{"ok":true}`) })
	c := s.client(t, M2, tok, Options{})
	const planted = "SECRET-planted-XYZ123"
	post := func(r Request) Request { r.Method = http.MethodPost; return r }
	cases := []struct {
		name string
		req  Request
	}{
		{"no route", Request{Method: "GET", Path: "/x"}},
		{"a route with capitals", getReq("M2.Report", "/x")},
		{"a route with a slash", getReq("m2/report", "/x")},
		{"a route with a hyphen (an id would sit there)", getReq("m2.report-123e4567", "/x")},
		{"a route longer than 64", getReq(strings.Repeat("a", 65), "/x")},
		{"an unknown method", Request{Route: "m2.x", Method: "TRACE", Path: "/x"}},
		{"no method", Request{Route: "m2.x", Path: "/x"}},
		{"no path", getReq("m2.x", "")},
		{"a path without a slash", getReq("m2.x", "v1/report")},
		{"a path with a query", getReq("m2.x", "/v1/report?user_id="+planted)},
		{"a path with a fragment", getReq("m2.x", "/v1/report#"+planted)},
		{"a path with a space", getReq("m2.x", "/v1/report "+planted)},
		{"a path with a control character", getReq("m2.x", "/v1/report\r\n"+planted)},
		{"a path with a backslash", getReq("m2.x", `/v1\`+planted)},
		{"a negative timeout", Request{Route: "m2.x", Method: "GET", Path: "/x", Timeout: -time.Second}},
		{"a timeout over the maximum", Request{Route: "m2.x", Method: "GET", Path: "/x", Timeout: MaxRequestTimeout + time.Second}},
		{"an Expect of 404", Request{Route: "m2.x", Method: "GET", Path: "/x", Expect: []int{404}}},
		{"an Expect of 302", Request{Route: "m2.x", Method: "GET", Path: "/x", Expect: []int{200, 302}}},
		{"a GET with a body", Request{Route: "m2.x", Method: "GET", Path: "/x", Body: map[string]string{"a": planted}}},
		{"a DELETE with a body", Request{Route: "m2.x", Method: "DELETE", Path: "/x", Body: map[string]string{"a": planted}}},
		{"a body that cannot be encoded", post(Request{Route: "m2.x", Path: "/x", Body: make(chan int)})},
		{"a request built for another module", Request{Module: M3, Route: "m2.x", Method: "GET", Path: "/x"}},
	}
	if len(cases) < 20 {
		t.Fatal("the table shrank")
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := s.count()
			resp, err := c.Do(ctxT(t, 5*time.Second), tc.req)
			if resp != nil {
				t.Fatalf("a value came back with %v", err)
			}
			e := wantKind(t, err, Invalid)
			if s.count() != before {
				t.Fatalf("a refused request still reached the network")
			}
			if text := fmt.Sprintf("%v %+v", err, e); containsAny(text, planted, "/v1/", tok) != "" {
				t.Fatalf("the error echoes the request: %s", text)
			}
		})
	}
	// The paired control: a sound request on the same client reaches the server.
	if _, err := c.Do(ctxT(t, 5*time.Second), getReq("m2.report", "/v1/report")); err != nil || s.count() != 1 {
		t.Fatalf("control failed: %v, hits %d", err, s.count())
	}
}

func TestDo_NeverFollowsARedirect(t *testing.T) {
	tok := mustToken(t)
	elsewhere := newStub(t, func(w http.ResponseWriter, r *http.Request, n int) { jsonReply(w, 200, `{"landed":true}`) })
	moved := newStub(t, func(w http.ResponseWriter, r *http.Request, n int) {
		http.Redirect(w, r, elsewhere.srv.URL+"/landing", http.StatusTemporaryRedirect) // a 307 keeps the method and the body
	})
	c := moved.client(t, M2, tok, Options{Retries: 3})
	resp, err := c.Do(ctxT(t, 5*time.Second), Request{Route: "m2.run", Method: http.MethodPost, Path: "/v1/run", Body: map[string]string{"user_id": "u-1"}, Idempotent: true})
	if resp != nil {
		t.Fatal("a value came back")
	}
	e := wantKind(t, err, Contract)
	if e.Status != http.StatusTemporaryRedirect || elsewhere.count() != 0 || moved.count() != 1 {
		t.Fatalf("status %d, the redirect target saw %d request(s), the module %d (a redirect is a Contract error and is never retried)", e.Status, elsewhere.count(), moved.count())
	}

	// The paired control: a plain http.Client follows the same 307 and re-sends the body to the other server, which is what the replaced CheckRedirect prevents.
	req, _ := http.NewRequest(http.MethodPost, moved.srv.URL+"/v1/run", bytes.NewReader([]byte(`{"user_id":"u-1"}`)))
	req.Header.Set("Authorization", "Bearer "+tok)
	naive, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = naive.Body.Close()
	if elsewhere.count() != 1 || !strings.Contains(string(elsewhere.requests()[0].body), "u-1") {
		t.Fatalf("control failed: a default client did not follow the redirect with its body (%d, %+v)", elsewhere.count(), elsewhere.requests())
	}
}

// roundTripFunc is an injected transport.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDo_TheSecretNeverAppearsInAnyErrorWhatEverTheModuleOrTheTransportSays(t *testing.T) {
	tok := mustToken(t)
	// A module (or something in front of it) that hands the credential back in every place a response has.
	echo := func(w http.ResponseWriter, status int, ctype, hdrID, hdrRetry, body string) {
		if ctype != "" {
			w.Header().Set("Content-Type", ctype)
		}
		if hdrID != "" {
			w.Header().Set("X-Request-Id", hdrID)
		}
		if hdrRetry != "" {
			w.Header().Set("Retry-After", hdrRetry)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
	env := func(code, msg, id, field, issue string) string {
		return `{"error":{"code":"` + code + `","message":"` + msg + `","details":[{"field":"` + field + `","issue":"` + issue + `"}],"request_id":"` + id + `"}}`
	}
	scenarios := []struct {
		name   string
		status int
		ctype  string
		hdrID  string
		retry  string
		body   string
	}{
		{"the secret as the error code", 409, "application/json", "", "", env(tok, "m", "r1", "f", "required")},
		{"the secret as the request id", 409, "application/json", "", "", env("conflict", "m", tok, "f", "required")},
		{"the secret as a detail field", 409, "application/json", "", "", env("conflict", "m", "r1", tok, "required")},
		{"the secret as a detail issue", 409, "application/json", "", "", env("conflict", "m", "r1", "f", tok)},
		{"the secret in the message", 409, "application/json", "", "", env("conflict", "Bearer "+tok, "r1", "f", "required")},
		{"the secret in the request id header", 500, "text/plain", tok, "", "boom"},
		{"the secret in a plain-text 500 body", 500, "text/plain", "", "", "Authorization: Bearer " + tok},
		{"the secret as a Retry-After", 429, "application/json", "", tok, env("rate_limited", "m", "r1", "f", "required")},
		{"the secret as a content type", 200, tok, "", "", `{"ok":true}`},
		{"the secret in a 404 body", 404, "text/html", "", "", "<html>Bearer " + tok + "</html>"},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			s := newStub(t, func(w http.ResponseWriter, r *http.Request, n int) {
				echo(w, sc.status, sc.ctype, sc.hdrID, sc.retry, sc.body)
			})
			// The control: the stub really does hand the secret back (so an error free of it is the client's doing).
			raw, err := http.Get(s.srv.URL + "/x")
			if err != nil {
				t.Fatal(err)
			}
			rb, _ := io.ReadAll(raw.Body)
			_ = raw.Body.Close()
			if containsAny(string(rb)+fmt.Sprint(raw.Header), tok) == "" {
				t.Fatalf("control failed: the stub does not echo the secret")
			}
			c := s.client(t, M2, tok, Options{})
			resp, err := c.Do(ctxT(t, 5*time.Second), getReq("m2.report", "/v1/report"))
			if resp != nil || err == nil {
				t.Fatalf("Do = %+v, %v", resp, err)
			}
			assertNoSecret(t, err, tok)
		})
	}

	t.Run("a transport error that names the credential", func(t *testing.T) {
		rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("transport: cannot send header %q to 203.0.113.9:443", r.Header.Get("Authorization"))
		})
		c, err := New(M2, "http://module.test:8080", config.Secret(tok), &http.Client{Transport: rt}, Options{})
		if err != nil {
			t.Fatal(err)
		}
		// The control: the injected transport's own error (and the *url.Error http.Client wraps it in) DO carry the credential.
		_, direct := (&http.Client{Transport: rt}).Do(mustReq(t, "http://module.test:8080/x", tok))
		if direct == nil || !strings.Contains(direct.Error(), tok) {
			t.Fatalf("control failed: the raw transport error does not carry the secret: %v", direct)
		}
		resp, err := c.Do(ctxT(t, 5*time.Second), getReq("m2.report", "/v1/report"))
		if resp != nil {
			t.Fatal("a value came back")
		}
		wantKind(t, err, Unavailable)
		assertNoSecret(t, err, tok, "203.0.113.9", "module.test")
	})
}

func mustReq(t testing.TB, u, tok string) *http.Request {
	t.Helper()
	r, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+tok)
	return r
}

// assertNoSecret renders err in every way and walks its chain: none of the needles may appear.
func assertNoSecret(t testing.TB, err error, needles ...string) {
	t.Helper()
	var texts []string
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d"} {
		texts = append(texts, fmt.Sprintf(verb, err))
	}
	for u := err; u != nil; u = errors.Unwrap(u) {
		texts = append(texts, u.Error())
	}
	if e := asErr(t, err); e != nil {
		texts = append(texts, fmt.Sprintf("%+v %#v %v %v", *e, *e, e.Details, e.Cause))
	}
	for _, text := range texts {
		if leaked := containsAny(text, needles...); leaked != "" {
			t.Fatalf("an error text contains %q: %s", leaked, text)
		}
	}
}

func TestClientAndRequestRenderWithoutTheirSecrets(t *testing.T) {
	tok := mustToken(t)
	c, err := New(M3, "http://module.test:8080", config.Secret(tok), nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	smtpSecret := mustToken(t)
	req := Request{Route: "m3.accounts.create", Method: http.MethodPost, Path: "/v1/send-byo/accounts/ca1e6e5f-0000-4000-8000-000000000001", Query: url.Values{"user_id": {"u-secret-id"}}, Body: map[string]string{"smtp_secret": smtpSecret}}

	// The paired controls: a plain struct with the same shape DOES print the secret (a config.Secret in an UNEXPORTED field is printed raw by fmt, which does not call methods
	// on unexported fields), and so does a plain request.
	type naive struct {
		base  string
		token config.Secret
	}
	type naiveReq struct {
		Path string
		Body any
	}
	for _, leaky := range []string{
		fmt.Sprintf("%+v", naive{"http://module.test:8080", config.Secret(tok)}),
		fmt.Sprintf("%v", naiveReq{req.Path, req.Body}),
	} {
		if containsAny(leaky, tok, smtpSecret, "ca1e6e5f") == "" {
			t.Fatalf("control failed: a plain struct did not leak (%s), so the proofs below prove nothing", leaky)
		}
	}

	subjects := map[string]any{
		"Client value": *c, "Client pointer": c, "in a slice": []any{*c, c}, "in a map": map[string]*Client{"a": c},
		"in a struct": struct{ C *Client }{c}, "a struct by value": struct{ C Client }{*c},
		"Request value": req, "Request pointer": &req, "Request in a slice": []Request{req},
	}
	n := 0
	for name, subject := range subjects {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%t", "%c", "%e", "%10.3v"} {
			out := fmt.Sprintf(verb, subject)
			n++
			if out == "" {
				t.Errorf("%s %s printed nothing: the check would pass over nothing", name, verb)
			}
			if leaked := containsAny(out, tok, smtpSecret, "ca1e6e5f", "u-secret-id"); leaked != "" {
				t.Errorf("%s %s reveals %q: %s", name, verb, leaked, out)
			}
		}
	}
	if n < 100 {
		t.Fatalf("only %d renderings were checked", n)
	}
	// Structured logging and JSON.
	var buf bytes.Buffer
	for _, h := range []slog.Handler{slog.NewJSONHandler(&buf, nil), slog.NewTextHandler(&buf, nil)} {
		l := slog.New(h)
		l.Info("client", "c", c, "v", *c, "req", req)
		l.Info("group", slog.Group("g", slog.Any("c", c)))
	}
	if buf.Len() == 0 || containsAny(buf.String(), tok, smtpSecret, "ca1e6e5f", "u-secret-id") != "" {
		t.Fatalf("slog output (%d bytes) reveals a secret or is empty: %s", buf.Len(), buf.String())
	}
	for _, v := range []any{c, *c, req, &req, []Request{req}, map[string]Request{"r": req}} {
		b, err := json.Marshal(v)
		if err != nil || len(b) == 0 || containsAny(string(b), tok, smtpSecret, "ca1e6e5f", "u-secret-id") != "" {
			t.Fatalf("json of a %T: %s, %v", v, b, err)
		}
	}
	// the paired control of the JSON sinks: a plain request struct DOES leak its body through encoding/json
	if b, _ := json.Marshal(naiveReq{req.Path, req.Body}); containsAny(string(b), smtpSecret) == "" {
		t.Fatalf("control failed: a plain request did not leak through JSON: %s", b)
	}
	if !strings.Contains(c.String(), "[redacted]") || !strings.Contains(c.String(), "module.test") {
		t.Fatalf("String() = %s", c.String())
	}
}
