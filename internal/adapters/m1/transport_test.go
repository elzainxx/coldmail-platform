package m1

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// submit202 is the 202 of module 1's own contract (bulk-item-identity.md section 3) with a uuid for its job id, for the plan of threeRowPlan (three rows, one repeat).
const submit202 = `{"job_id":"` + testJob + `","status":"accepted","counts":{"total":3,"enqueued":2,"duplicate":1,"malformed":0},"malformed_rows":[],"malformed_rows_truncated":false,"processing_paused":false}`

func threeRowPlan() Plan {
	return Prepare([]Input{{"row-0", "jane@acmecorp.com"}, {"row-1", "bob@acmecorp.com"}, {"row-2", "Jane@AcmeCorp.com"}})
}

// TestErrors_NameStatusAndRouteNeverTheKeyOrAnAddress: a hostile module 1 that puts the Authorization header and an address into every field of every answer. Whatever the status
// (5xx, 401, 404, 400, a redirect, a 200 with a garbage body), the error names the status and the route and carries neither the key nor the address; module 1's own error.code is
// kept only when it is a short word of the safe alphabet. Control: leaks() sees the key in a string that holds it, and every scenario's body really does hold both secrets.
func TestErrors_NameStatusAndRouteNeverTheKeyOrAnAddress(t *testing.T) {
	key := newToken(t)
	hostile := func(code string) string {
		b, _ := json.Marshal(map[string]any{
			"error":   map[string]any{"code": code, "message": "Bearer " + key + " " + testAddr, "details": []map[string]string{{"field": testAddr, "issue": key}}, "request_id": key},
			"job_id":  testAddr,
			"results": []map[string]string{{"email": testAddr, "item_id": key}},
		})
		return string(b)
	}
	type sc struct {
		name   string
		status int
		body   string
		call   string // submit, status, results
		want   string // what the error text must contain
	}
	scenarios := []sc{
		{"submit 500", 500, hostile("internal_error"), "submit", "HTTP 500"},
		{"status 503", 503, hostile("unavailable"), "status", "HTTP 503"},
		{"results 500 with the key as its code", 500, hostile(key), "results", "HTTP 500"},
		{"results 401", 401, hostile("unauthenticated"), "results", "HTTP 401"},
		{"status 404", 404, hostile("not_found"), "status", "HTTP 404"},
		{"submit 400", 400, hostile("validation_failed"), "submit", "HTTP 400"},
		{"submit 413", 413, hostile("payload_too_large"), "submit", "HTTP 413"},
		{"results 302", 302, hostile("x"), "results", "HTTP 302"},
		{"status 200 garbage", 200, "Bearer " + key + " " + testAddr, "status", "not JSON"},
		{"results 200 garbage", 200, "{" + key + " " + testAddr, "results", "not JSON"},
	}
	routes := map[string]string{"submit": routeSubmit, "status": routeStatus, "results": routeResults}
	for _, s := range scenarios {
		if !leaks(s.body, key) || !leaks(s.body, testAddr) {
			t.Fatalf("%s: the hostile body does not hold both secrets, so the scenario proves nothing", s.name)
		}
		f := serve(t, s.status, s.body)
		a, err := New(f.srv.URL, config.Secret(key), nil, modclient.Options{Sleep: noWait})
		if err != nil {
			t.Fatal(err)
		}
		var cerr error
		switch s.call {
		case "submit":
			_, cerr = a.Submit(testCtx(t), threeRowPlan(), newIdemKey(t))
		case "status":
			_, cerr = a.Status(testCtx(t), testJob)
		case "results":
			_, _, cerr = a.Results(testCtx(t), testJob, "", 0)
		}
		if cerr == nil {
			t.Errorf("%s: no error", s.name)
			continue
		}
		text := cerr.Error()
		if leaks(text, key, testAddr, "leak-check") {
			t.Errorf("%s: the error carries the key or an address: %q", s.name, text)
		}
		if !strings.Contains(text, routes[s.call]) || !strings.Contains(text, s.want) {
			t.Errorf("%s: the error %q does not name the route %q and %q", s.name, text, routes[s.call], s.want)
		}
	}
	f := serve(t, 500, hostile("internal_error"))
	a, _ := New(f.srv.URL, config.Secret(key), nil, modclient.Options{})
	if _, err := a.Status(testCtx(t), testJob); asModclient(err) == nil || asModclient(err).Code != "internal_error" || asModclient(err).Kind != modclient.Internal {
		t.Fatalf("module 1's own code was not kept: %v", err)
	}
	if !leaks("Bearer "+key, key) || leaks("fine", key) {
		t.Fatal("control: leaks() does not behave: the checks above prove nothing")
	}
}

// TestResponses_AreSizeLimited: a body of exactly the limit is read, one byte more is a Contract error, and an oversized ERROR answer is still reported by its status (with no code
// taken from a body that was not read in full). The limit is Options.MaxBodyBytes; the adapter sets none of its own. Control: the at-limit body decodes to its (empty) page.
func TestResponses_AreSizeLimited(t *testing.T) {
	const limit = 4096
	const head = `{"job_id":"` + testJob + `","results":[],"next_cursor":"","pad":"`
	const tail = `"}`
	padTo := func(size int) string { return head + strings.Repeat("a", size-len(head)-len(tail)) + tail }
	f := newFakeM1(t, func(c call) (int, string) {
		switch c.params().Get("cursor") {
		case "big":
			return 200, padTo(limit + 1)
		case "err":
			return 500, `{"error":{"code":"internal_error"},"pad":"` + strings.Repeat("a", limit) + `"}`
		}
		return 200, padTo(limit)
	})
	a := f.adapterWith(t, modclient.Options{MaxBodyBytes: limit, Sleep: noWait})
	if rows, next, err := a.Results(testCtx(t), testJob, "", 0); err != nil || len(rows) != 0 || next != "" {
		t.Fatalf("control: a body of exactly the limit: %v %q %v", rows, next, err)
	}
	if _, _, err := a.Results(testCtx(t), testJob, "big", 0); modclient.KindOf(err) != modclient.Contract {
		t.Fatalf("a body of limit+1: got %v, want a Contract error", err)
	}
	e := func() *modclient.Error {
		_, _, err := a.Results(testCtx(t), testJob, "err", 0)
		return asModclient(err)
	}()
	if e == nil || e.Status != 500 || e.Code != "" || e.Kind != modclient.Internal {
		t.Fatalf("an oversized 500: got %+v, want an Internal error 500 with no code", e)
	}
}

// TestTransportFailures_AreClassifiedNotQuoted: a connection refused is Unavailable and a stalled module is a Timeout; the text names the route and never the host or port; the
// timeout unwraps to context.DeadlineExceeded. Control: leaks() sees the host in the raw error of the same failure.
func TestTransportFailures_AreClassifiedNotQuoted(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	hostport := strings.TrimPrefix(dead.URL, "http://")
	dead.Close()
	key := newToken(t)
	a, err := New(dead.URL, config.Secret(key), nil, modclient.Options{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.Status(testCtx(t), testJob)
	if e := asModclient(err); e == nil || e.Kind != modclient.Unavailable || e.Route != routeStatus || e.Sent {
		t.Fatalf("a closed port: got %v, want an Unavailable error on m1.status that never sent", err)
	}
	if leaks(err.Error(), hostport, "127.0.0.1", key) {
		t.Fatalf("the error quotes the address it dialled: %q", err)
	}
	// control: the raw error of the same failure does carry it
	raw, rerr := http.Get(dead.URL + "/x")
	if rerr == nil {
		_ = raw.Body.Close()
		t.Fatal("control: the closed server answered")
	}
	if !leaks(rerr.Error(), hostport) {
		t.Fatalf("control: the standard library's own error does not hold the host, so the check above proves nothing: %q", rerr)
	}

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	}))
	defer slow.Close()
	sa, err := New(slow.URL, config.Secret(key), nil, modclient.Options{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = sa.Status(ctxFor(t, 100*time.Millisecond), testJob)
	if e := asModclient(err); e == nil || e.Kind != modclient.Timeout || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a hanging module: got %v, want a Timeout error that unwraps to DeadlineExceeded", err)
	}
	if leaks(err.Error(), strings.TrimPrefix(slow.URL, "http://"), key) {
		t.Fatalf("the timeout error quotes the address it dialled: %q", err)
	}
}

// TestRedirects_AreNeverFollowed: a 302 is a Contract error and the place it points at is never contacted, even when the caller supplies an http.Client that follows redirects (a
// redirect would re-send the key). Control: that very client, used directly, does follow it, so the target is reachable and the assertion could have failed.
func TestRedirects_AreNeverFollowed(t *testing.T) {
	var hits int
	var mu sync.Mutex
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		_, _ = io.WriteString(w, `{}`)
	}))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/elsewhere", http.StatusFound)
	}))
	defer redirector.Close()

	following := &http.Client{} // the default policy: follows up to ten redirects
	a, err := New(redirector.URL, config.Secret(newToken(t)), following, modclient.Options{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.Status(testCtx(t), testJob)
	if e := asModclient(err); e == nil || e.Status != 302 || e.Kind != modclient.Contract {
		t.Fatalf("got %v, want a Contract error for the 302", err)
	}
	mu.Lock()
	got := hits
	mu.Unlock()
	if got != 0 {
		t.Fatalf("the redirect target was contacted %d time(s): the key and the request were forwarded", got)
	}
	if following.CheckRedirect != nil {
		t.Fatal("New modified the caller's http.Client instead of copying it")
	}
	resp, err := following.Get(redirector.URL + "/x")
	if err != nil {
		t.Fatalf("control: %v", err)
	}
	_ = resp.Body.Close()
	mu.Lock()
	got = hits
	mu.Unlock()
	if got != 1 {
		t.Fatalf("control: the client that follows redirects reached the target %d time(s), want 1", got)
	}
}

// TestNew_ValidatesItsInputs: a base URL that is not plain http(s) with a host, or that carries a credential, query or fragment, and a key that is empty or has whitespace, control or
// non-ASCII characters are refused with an Invalid error, and the refusal quotes neither. Control: plain URLs and a hex key build, a trailing slash is trimmed.
func TestNew_ValidatesItsInputs(t *testing.T) {
	good := newToken(t)
	for _, u := range []string{"", "ftp://m1:8080", "http://", "http://user:pw-secret@m1:8080", "http://m1:8080?x=pw-secret", "http://m1:8080#pw-secret", "http://m1:8080?", "mailto:pw-secret@m1", "m1:8080"} {
		_, err := New(u, config.Secret(good), nil, modclient.Options{})
		if modclient.KindOf(err) != modclient.Invalid {
			t.Errorf("base URL %q: got %v, want an Invalid error", u, err)
			continue
		}
		if leaks(err.Error(), "pw-secret", good) {
			t.Errorf("base URL %q: the error quotes it: %q", u, err)
		}
	}
	for _, k := range []string{"", "pwx-has space", "pwx-tab\there", "pwx-new\nline", "pwx-café", " pwx-lead", "pwx-trail\r"} {
		a, err := New("http://m1:8080", config.Secret(k), nil, modclient.Options{})
		if a != nil || modclient.KindOf(err) != modclient.Invalid {
			t.Errorf("key %q: got %v, want an Invalid error and no adapter", k, err)
			continue
		}
		if leaks(err.Error(), "pwx-") {
			t.Errorf("key %q: the error quotes it: %q", k, err)
		}
	}
	if _, err := New("http://m1:8080", config.Secret("pwx-0123456789abcdef"), nil, modclient.Options{}); err != nil {
		t.Errorf("control: a plain hex-ish key was refused: %v", err)
	}
	for _, u := range []string{"http://m1:8080", "http://m1:8080/", "https://m1.example/prefix/"} {
		a, err := New(u, config.Secret("0123456789abcdef0123456789abcdef"), nil, modclient.Options{})
		if err != nil {
			t.Errorf("control: %q was refused: %v", u, err)
			continue
		}
		if strings.Contains(a.String(), "/,") || strings.Contains(a.String(), "/ ") {
			t.Errorf("control: the base %q was not trimmed of its trailing slash: %s", u, a)
		}
	}
	if _, err := New("http://m1:8080", config.Secret(good), nil, modclient.Options{Retries: -1}); modclient.KindOf(err) != modclient.Invalid {
		t.Errorf("bad dials: got %v, want an Invalid error", err)
	}
}

// TestKey_IsRedactedForEveryVerb: the Adapter prints without its key under every fmt verb, as a value and as a pointer, and encoding/json (the exported fields) does not carry it.
// Control: a struct holding the same key as a plain string does print it, so the check can see a leak.
func TestKey_IsRedactedForEveryVerb(t *testing.T) {
	key := newToken(t)
	a, err := New("http://m1:8080", config.Secret(key), nil, modclient.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []any{a, *a} {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%d", "%x"} {
			out := fmt.Sprintf(verb, v)
			if strings.Contains(out, key) || strings.Contains(out, fmt.Sprintf("%x", key)) {
				t.Errorf("%s of a %T prints the key: %q", verb, v, out)
			}
		}
		if out := fmt.Sprint(v); !strings.Contains(out, "[redacted]") {
			t.Errorf("a %T renders as %q, want it to say the key is redacted", v, out)
		}
	}
	if b, err := json.Marshal(a); err != nil || leaks(string(b), key) {
		t.Errorf("json.Marshal of the Adapter: %s, %v", b, err)
	}
	if out := fmt.Sprintf("%v", struct{ Key string }{key}); !leaks(out, key) {
		t.Fatal("control: a plain string field does not print the key, so the checks above prove nothing")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestEveryCallHasADeadline: a call whose context has no deadline still gets one (the dial's, or the longer floor of the two calls that need it: Submit and Verify); a call whose
// context has one keeps it when it is the earlier. Checked for all four calls on the request's own context. Control: the contexts the test hands in really differ in having a
// deadline.
func TestEveryCallHasADeadline(t *testing.T) {
	var mu sync.Mutex
	deadlines := map[string]time.Time{}
	missing := map[string]bool{}
	body := func(r *http.Request) (int, string) {
		switch {
		case r.URL.Path == "/v1/verify":
			return 200, fixture(t, "verify-ok.json")
		case strings.HasSuffix(r.URL.Path, "/results"):
			return 200, page(testJob, "")
		case r.Method == http.MethodPost:
			return 202, submit202
		}
		return 200, statusBody(testJob, "running", false, 1, 0)
	}
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		dl, ok := r.Context().Deadline()
		mu.Lock()
		name := r.Method + " " + r.URL.Path
		if strings.HasPrefix(r.URL.Path, "/v1/jobs/") && !strings.HasSuffix(r.URL.Path, "/results") {
			name = "status"
		}
		if ok {
			deadlines[name] = dl
		} else {
			missing[name] = true
		}
		mu.Unlock()
		status, b := body(r)
		h := http.Header{}
		h.Set("Content-Type", "application/json")
		return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(b)), Request: r}, nil
	})}
	a, err := New("http://m1.invalid:8080", config.Secret(newToken(t)), hc, modclient.Options{Timeout: 20 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	calls := map[string]func(ctx context.Context) error{
		"POST /v1/verify/bulk": func(ctx context.Context) error { _, err := a.Submit(ctx, threeRowPlan(), newIdemKey(t)); return err },
		"POST /v1/verify":      func(ctx context.Context) error { _, err := a.Verify(ctx, "ada@acme.example"); return err },
		"status":               func(ctx context.Context) error { _, err := a.Status(ctx, testJob); return err },
		"GET /v1/jobs/" + testJob + "/results": func(ctx context.Context) error {
			_, _, err := a.Results(ctx, testJob, "", 0)
			return err
		},
	}
	if _, ok := context.Background().Deadline(); ok {
		t.Fatal("control: context.Background() has a deadline")
	}
	start := time.Now()
	for name, fn := range calls {
		if err := fn(context.Background()); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	mu.Lock()
	if len(missing) != 0 || len(deadlines) != len(calls) {
		t.Fatalf("with no deadline in the context: %v had none, %d had one, want none and %d", missing, len(deadlines), len(calls))
	}
	want := map[string]time.Duration{"POST /v1/verify/bulk": submitTimeoutFloor, "POST /v1/verify": verifyTimeoutFloor, "status": 20 * time.Second, "GET /v1/jobs/" + testJob + "/results": 20 * time.Second}
	for name, dl := range deadlines {
		if d := dl.Sub(start); d < want[name]-2*time.Second || d > want[name]+2*time.Second {
			t.Errorf("%s: the default deadline is %s ahead, want about %s", name, d, want[name])
		}
	}
	for k := range deadlines {
		delete(deadlines, k)
	}
	mu.Unlock()

	short := ctxFor(t, 5*time.Second)
	start = time.Now()
	for name, fn := range calls {
		if err := fn(short); err != nil {
			t.Fatalf("%s with a deadline: %v", name, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for name, dl := range deadlines {
		if d := dl.Sub(start); d > 6*time.Second {
			t.Errorf("%s: the caller's 5 s deadline became %s ahead: it was replaced by a longer one", name, d)
		}
	}
	if len(deadlines) != len(calls) {
		t.Fatalf("%d request(s) with a deadline, want %d", len(deadlines), len(calls))
	}
}

// TestBadShapes_AreRefused: a 2xx answer that is not the documented shape is a Contract error naming the key (never a half-read result), for each route: no job_id, no complete
// flag, no counts, no results array, a page about another job, text after the JSON value, a body that is not JSON; and a 200 where the route answers 202 is a Contract error too.
// Control: the documented bodies of the same routes are accepted.
func TestBadShapes_AreRefused(t *testing.T) {
	ctx := testCtx(t)
	submit := func(status int, body string) error {
		_, err := serve(t, status, body).adapter(t).Submit(ctx, threeRowPlan(), newIdemKey(t))
		return err
	}
	status := func(body string) error {
		_, err := serve(t, 200, body).adapter(t).Status(ctx, testJob)
		return err
	}
	results := func(body string) error {
		_, _, err := serve(t, 200, body).adapter(t).Results(ctx, testJob, "", 0)
		return err
	}
	for name, err := range map[string]error{
		"submit: control (the documented 202)":  submit(202, submit202),
		"status: control (the documented body)": status(completeBody(3)),
		"results: control (a page)":             results(page(testJob, "", row(0))),
	} {
		if err != nil {
			t.Errorf("%s was refused: %v", name, err)
		}
	}
	running := statusBody(testJob, "running", false, 1, 0) // a good status body that the cases below take one key out of
	type bad struct {
		err error
		key string
	}
	cases := map[string]bad{
		"submit: no job_id":                   {submit(202, `{"status":"accepted","counts":{"total":3,"enqueued":2,"duplicate":1,"malformed":0}}`), "job_id"},
		"submit: no counts":                   {submit(202, `{"job_id":"`+testJob+`","status":"accepted"}`), "counts"},
		"submit: a counts object without one": {submit(202, `{"job_id":"`+testJob+`","status":"accepted","counts":{"total":3,"enqueued":2,"duplicate":1}}`), "counts.malformed"},
		"submit: not JSON":                    {submit(202, `accepted`), ""},
		"status: no complete flag":            {status(remove("complete").apply(t, running)), "complete"},
		"status: no counts":                   {status(remove("counts").apply(t, running)), "counts"},
		"status: counts without pending":      {status(remove("counts.pending").apply(t, running)), "counts.pending"},
		"status: no totals":                   {status(remove("totals").apply(t, running)), "totals"},
		"status: totals without by_status":    {status(remove("totals.by_status").apply(t, running)), "totals.by_status"},
		"status: totals without submitted":    {status(remove("totals.submitted").apply(t, running)), "totals.submitted"},
		"status: no processing_paused":        {status(remove("processing_paused").apply(t, running)), "processing_paused"},
		"status: another job":                 {status(statusBody("00000000-0000-7000-8000-0000000000ff", "complete", true, 1, 1)), "job_id"},
		"results: no results key":             {results(`{"job_id":"` + testJob + `","next_cursor":""}`), "results"},
		"results: results is null":            {results(`{"job_id":"` + testJob + `","results":null,"next_cursor":""}`), "results"},
		"results: no next_cursor":             {results(`{"job_id":"` + testJob + `","results":[]}`), "next_cursor"},
		"results: text after the value":       {results(page(testJob, "") + ` {"again":1}`), ""},
		"results: not JSON":                   {results(`<html>`), ""},
	}
	for name, c := range cases {
		kind, key := kindAndKey(c.err)
		if kind != modclient.Contract || key != c.key {
			t.Errorf("%s: got kind %v key %q (%v), want a Contract error naming %q", name, kind, key, c.err, c.key)
		}
	}
	// a 200 where the route answers 202 is a status the route does not use: Contract, and the module may have created something, so the message says the same key may be sent again
	err := submit(200, submit202)
	if e := asModclient(err); e == nil || e.Kind != modclient.Contract || e.Status != 200 || !strings.Contains(err.Error(), unconfirmedPhrase) {
		t.Errorf("a 200 where the route answers 202: got %v, want a Contract error (HTTP 200) that says the same key may be sent again", err)
	}
}
