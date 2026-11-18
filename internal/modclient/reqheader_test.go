package modclient

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// Request.Header (Phase 3 close-out): the one way a route's contract header beyond the credential, module 1's `Idempotency-Key`, reaches the module. It is additive: a request with
// no Header is sent exactly as before.

// headerSeen is what one attempt of a stub carried.
type headerSeen struct{ key, auth, ctype, other string }

func recordHeaders(mu *sync.Mutex, into *[]headerSeen, name string) func(w http.ResponseWriter, r *http.Request, n int) {
	return func(w http.ResponseWriter, r *http.Request, n int) {
		mu.Lock()
		*into = append(*into, headerSeen{r.Header.Get(name), r.Header.Get("Authorization"), r.Header.Get("Content-Type"), r.Header.Get("X-Other")})
		mu.Unlock()
		if n == 1 {
			jsonReply(w, 503, m1NotWired)
			return
		}
		jsonReply(w, 202, `{"ok":true}`)
	}
}

// TestRequestHeader_IsSentOnEveryAttempt: a Request with Header {Idempotency-Key: k} sends it, with the credential and the JSON content type as before, on EVERY attempt of a retried
// request (the first attempt gets a 503, the second the answer: both carry the same key, which is what makes the retry a repeat of one request for the module). Paired controls: the same
// request with no Header sends no such header and nothing else changes; a request NOT marked Idempotent sends the header once and is not retried (the header does not make a request
// retryable; the caller's Idempotent flag does); and a second header is sent beside the first.
func TestRequestHeader_IsSentOnEveryAttempt(t *testing.T) {
	tok := mustToken(t)
	const key = "run-42.job:7_a-b"
	req := func(h map[string]string, idempotent bool) Request {
		return Request{Route: "m1.submit", Method: http.MethodPost, Path: "/v1/verify/bulk", Body: map[string]int{"a": 1}, Expect: []int{202}, Idempotent: idempotent, Header: h}
	}
	var mu sync.Mutex
	var seen []headerSeen
	s := newStub(t, recordHeaders(&mu, &seen, "Idempotency-Key"))
	c := s.client(t, M1, tok, Options{Retries: 2, Sleep: (&recorder{}).sleep})
	resp, err := c.Do(ctxT(t, 5*time.Second), req(map[string]string{"Idempotency-Key": key, "X-Other": "two"}, true))
	if err != nil || resp == nil || resp.Attempts != 2 || s.count() != 2 {
		t.Fatalf("%v, %+v, %d request(s), want the answer on the second attempt", err, resp, s.count())
	}
	mu.Lock()
	for i, h := range seen {
		if h.key != key || h.auth != "Bearer "+tok || h.ctype != "application/json" || h.other != "two" {
			t.Errorf("attempt %d carried %+v, want the same key, the credential, the content type and the second header", i+1, h)
		}
	}
	mu.Unlock()

	// control: no Header, no such header, and the rest of the request is what it was
	mu.Lock()
	seen = nil
	mu.Unlock()
	s2 := newStub(t, recordHeaders(&mu, &seen, "Idempotency-Key"))
	c2 := s2.client(t, M1, tok, Options{Retries: 2, Sleep: (&recorder{}).sleep})
	if _, err := c2.Do(ctxT(t, 5*time.Second), req(nil, true)); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	for i, h := range seen {
		if h.key != "" || h.other != "" || h.auth != "Bearer "+tok || h.ctype != "application/json" {
			t.Errorf("control, attempt %d carried %+v, want only the credential and the content type", i+1, h)
		}
	}
	if len(seen) != 2 {
		t.Errorf("control: %d attempt(s), want 2", len(seen))
	}
	seen = nil
	mu.Unlock()

	// control: the header does not make a request retryable
	s3 := newStub(t, recordHeaders(&mu, &seen, "Idempotency-Key"))
	c3 := s3.client(t, M1, tok, Options{Retries: 2, Sleep: (&recorder{}).sleep})
	if r, err := c3.Do(ctxT(t, 5*time.Second), req(map[string]string{"Idempotency-Key": key}, false)); r != nil {
		t.Fatalf("a request that is not idempotent was retried to success: %v", err)
	} else {
		wantKind(t, err, Unavailable)
	}
	mu.Lock()
	if len(seen) != 1 || seen[0].key != key {
		t.Errorf("a request that is not marked Idempotent made %d attempt(s) (%+v), want exactly one, with the header", len(seen), seen)
	}
	mu.Unlock()
}

// TestResponse_ExposesTheModulesHeaders: the headers of a success answer are on Response.Header (module 1 marks the replay of an idempotent create with
// `Idempotent-Replayed: true`, which only the header carries). Paired control: an answer without the header has none, and Header is a copy (changing it does not touch what a second
// call returns).
func TestResponse_ExposesTheModulesHeaders(t *testing.T) {
	tok := mustToken(t)
	s := newStub(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if n == 1 {
			w.Header().Set("Idempotent-Replayed", "true")
		}
		jsonReply(w, 202, `{"ok":true}`)
	})
	c := s.client(t, M1, tok, Options{})
	req := Request{Route: "m1.submit", Method: http.MethodPost, Path: "/v1/verify/bulk", Body: map[string]int{"a": 1}, Expect: []int{202}}
	first, err := c.Do(ctxT(t, 5*time.Second), req)
	if err != nil || first.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("%v: the replay header is %q, want true", err, first.Header.Get("Idempotent-Replayed"))
	}
	first.Header.Set("Idempotent-Replayed", "changed")
	second, err := c.Do(ctxT(t, 5*time.Second), req)
	if err != nil || second.Header.Get("Idempotent-Replayed") != "" {
		t.Fatalf("control: %v: an answer without the header reads %q", err, second.Header.Get("Idempotent-Replayed"))
	}
}

// TestRequestHeader_AnUnsafeHeaderIsRefusedBeforeAnythingIsSent: a header the client owns (the credential, the content type and length, accept, user agent, host, framing), a name that is
// not letters, digits and '-', a value that is empty, has a space, a control character or a non-ASCII character, or is over 256 bytes, more than eight headers, and one name given
// twice in two cases are each an Invalid refusal with no request made, and the error quotes neither the name nor the value. Paired controls: a header of the allowed shape is sent;
// a value of exactly 256 characters and eight headers are accepted (the limits are where they are said to be).
func TestRequestHeader_AnUnsafeHeaderIsRefusedBeforeAnythingIsSent(t *testing.T) {
	tok := mustToken(t)
	const probe = "Zq9-probe-value"
	many := map[string]string{}
	for _, n := range []string{"X-A", "X-B", "X-C", "X-D", "X-E", "X-F", "X-G", "X-H", "X-I"} {
		many[n] = probe
	}
	rows := []struct {
		name string
		h    map[string]string
	}{
		{"the credential", map[string]string{"Authorization": probe}},
		{"the credential in another case", map[string]string{"authorization": probe}},
		{"the content type", map[string]string{"Content-Type": probe}},
		{"the content length", map[string]string{"Content-Length": "1"}},
		{"accept", map[string]string{"Accept": probe}},
		{"the user agent", map[string]string{"User-Agent": probe}},
		{"host", map[string]string{"Host": probe}},
		{"transfer encoding", map[string]string{"Transfer-Encoding": "chunked"}},
		{"a proxy credential", map[string]string{"Proxy-Authorization": probe}},
		{"a name with a space", map[string]string{"X Bad": probe}},
		{"a name with a colon", map[string]string{"X:Bad": probe}},
		{"an empty name", map[string]string{"": probe}},
		{"a name that is not ASCII", map[string]string{"X-Ünï": probe}},
		{"an empty value", map[string]string{"X-Key": ""}},
		{"a value with a space", map[string]string{"X-Key": "two words"}},
		{"a value with a line break", map[string]string{"X-Key": "a\r\nX-Injected: " + probe}},
		{"a value with a tab", map[string]string{"X-Key": "a\tb"}},
		{"a value that is not ASCII", map[string]string{"X-Key": "café"}},
		{"a value of 257 bytes", map[string]string{"X-Key": strings.Repeat("k", 257)}},
		{"nine headers", many},
		{"one name in two cases", map[string]string{"X-Key": probe, "x-key": probe}},
	}
	var seen []headerSeen
	var mu sync.Mutex
	s := newStub(t, recordHeaders(&mu, &seen, "X-Key"))
	c := s.client(t, M1, tok, Options{Sleep: (&recorder{}).sleep})
	for _, row := range rows {
		r, err := c.Do(ctxT(t, 5*time.Second), Request{Route: "m1.submit", Method: http.MethodPost, Path: "/v1/verify/bulk", Body: map[string]int{"a": 1}, Expect: []int{202}, Header: row.h})
		if r != nil {
			t.Errorf("%s: a response came back", row.name)
			continue
		}
		e := wantKind(t, err, Invalid)
		if e.Sent || e.Route != "m1.submit" {
			t.Errorf("%s: %+v", row.name, *e)
		}
		if containsAny(err.Error(), probe, "X Bad", "X:Bad", "caf") != "" {
			t.Errorf("%s: the refusal quotes what it refused: %q", row.name, err)
		}
	}
	if s.count() != 0 {
		t.Fatalf("%d request(s) were made for refused headers", s.count())
	}

	// controls: the limits are where they are said to be, and a header of the allowed shape is sent
	ok := newStub(t, func(w http.ResponseWriter, r *http.Request, n int) {
		mu.Lock()
		seen = append(seen, headerSeen{key: r.Header.Get("X-Key"), other: strings.Repeat("h", len(r.Header.Values("X-H1")))})
		mu.Unlock()
		jsonReply(w, 202, `{"ok":true}`)
	})
	oc := ok.client(t, M1, tok, Options{})
	eight := map[string]string{"X-H1": "1", "X-H2": "2", "X-H3": "3", "X-H4": "4", "X-H5": "5", "X-H6": "6", "X-H7": "7", "X-Key": strings.Repeat("k", 256)}
	if r, err := oc.Do(ctxT(t, 5*time.Second), Request{Route: "m1.submit", Method: http.MethodPost, Path: "/v1/verify/bulk", Body: map[string]int{"a": 1}, Expect: []int{202}, Header: eight}); err != nil || r == nil {
		t.Fatalf("control: eight headers with a value of 256 characters were refused: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || seen[0].key != strings.Repeat("k", 256) || seen[0].other != "h" {
		t.Fatalf("control: the request carried %+v", seen)
	}
}
