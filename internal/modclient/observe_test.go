package modclient

import (
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/config"
)

// collector is an Options.Observe that keeps what it was given.
type collector struct {
	mu   sync.Mutex
	seen []Observation
}

func (c *collector) observe(o Observation) {
	c.mu.Lock()
	c.seen = append(c.seen, o)
	c.mu.Unlock()
}

func (c *collector) got() []Observation {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Observation(nil), c.seen...)
}

func TestObserve_SeesEveryStatusClassWithTheBodyAndTheRequestAsSent(t *testing.T) {
	tok := mustToken(t)
	cases := []struct {
		name   string
		status int
		body   string
		kind   Kind // 0: a success
	}{
		{"success", 200, `{"ok":true}`, 0},
		{"client error with the module's envelope", 404, m2NotFound, NotFound},
		{"validation error", 400, m2Validation, Refused},
		{"server error", 500, m2Internal, Internal},
		{"paused", 503, m2Paused, Paused},
		{"rate limited", 429, m3RateLimited, RateLimited},
		{"a status the route does not use", 201, `{"ok":true}`, Contract},
		{"not JSON at all", 200, `<html>nope</html>`, Contract},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newStub(t, func(w http.ResponseWriter, r *http.Request, n int) { jsonReply(w, c.status, c.body) })
			col := &collector{}
			cl := s.client(t, M2, tok, Options{Observe: col.observe})
			resp, err := cl.Do(ctxT(t, 5*time.Second), getReq("m2.report", "/v1/jobs/abc123"))
			if c.kind == 0 {
				if err != nil || resp == nil {
					t.Fatalf("a success: %v", err)
				}
			} else {
				wantKind(t, err, c.kind)
			}
			got := col.got()
			if len(got) != 1 {
				t.Fatalf("observations = %d, want 1", len(got))
			}
			want := Observation{Module: M2, Route: "m2.report", Method: "GET", Path: "/v1/jobs/abc123", Status: c.status, Body: []byte(c.body)}
			if !reflect.DeepEqual(got[0], want) {
				t.Fatalf("observation = %+v, want %+v", got[0], want)
			}
		})
	}
}

func TestObserve_TheObserverDoesNotChangeWhatTheCallReturns(t *testing.T) {
	tok := mustToken(t)
	for _, c := range []struct {
		status int
		body   string
	}{{200, `{"a":1}`}, {404, m2NotFound}, {500, m2Internal}, {200, `garbage`}} {
		s := newStub(t, func(w http.ResponseWriter, r *http.Request, n int) { jsonReply(w, c.status, c.body) })
		plain := s.client(t, M2, tok, Options{})
		observed := s.client(t, M2, tok, Options{Observe: (&collector{}).observe})
		r1, e1 := plain.Do(ctxT(t, 5*time.Second), getReq("m2.report", "/v1/x"))
		r2, e2 := observed.Do(ctxT(t, 5*time.Second), getReq("m2.report", "/v1/x"))
		if (e1 == nil) != (e2 == nil) || KindOf(e1) != KindOf(e2) {
			t.Fatalf("status %d: with an observer %v, without %v", c.status, e2, e1)
		}
		if e1 == nil && (r1.Status != r2.Status || string(r1.Body) != string(r2.Body)) {
			t.Fatalf("the response differs with an observer")
		}
	}
}

func TestObserve_ATransportFailureProducesNoObservation(t *testing.T) {
	tok := mustToken(t)
	s := newStub(t, func(w http.ResponseWriter, r *http.Request, n int) { jsonReply(w, 200, `{}`) })
	col := &collector{}
	cl := s.client(t, M2, tok, Options{Observe: col.observe})
	if _, err := cl.Do(ctxT(t, 5*time.Second), getReq("m2.report", "/v1/x")); err != nil || len(col.got()) != 1 {
		t.Fatalf("the control: a live server is observed once, got %d (%v)", len(col.got()), err)
	}
	s.srv.Close() // nothing listens any more: connection refused
	_, err := cl.Do(ctxT(t, 5*time.Second), getReq("m2.report", "/v1/x"))
	wantKind(t, err, Unavailable)
	if n := len(col.got()); n != 1 {
		t.Fatalf("a transport failure must produce no observation, observations = %d", n)
	}
}

func TestObserve_ACallRefusedBeforeItIsSentProducesNoObservation(t *testing.T) {
	tok := mustToken(t)
	s := newStub(t, func(w http.ResponseWriter, r *http.Request, n int) { jsonReply(w, 200, `{}`) })
	col := &collector{}
	cl := s.client(t, M2, tok, Options{Observe: col.observe})
	_, err := cl.Do(ctxT(t, 5*time.Second), Request{Route: "Bad Route", Method: "GET", Path: "/v1/x"})
	wantKind(t, err, Invalid)
	if len(col.got()) != 0 || s.count() != 0 {
		t.Fatalf("nothing was sent, so nothing can be observed: %d observations, %d hits", len(col.got()), s.count())
	}
}

func TestObserve_APanickingObserverDoesNotChangeTheResult(t *testing.T) {
	tok := mustToken(t)
	for _, status := range []int{200, 404, 500} {
		body := `{"a":1}`
		if status == 404 {
			body = m2NotFound
		} else if status == 500 {
			body = m2Internal
		}
		s := newStub(t, func(w http.ResponseWriter, r *http.Request, n int) { jsonReply(w, status, body) })
		calls := 0
		cl := s.client(t, M2, tok, Options{Observe: func(Observation) { calls++; panic("the observer fails") }})
		resp, err := cl.Do(ctxT(t, 5*time.Second), getReq("m2.report", "/v1/x"))
		if calls != 1 {
			t.Fatalf("status %d: the observer was called %d times, want 1", status, calls)
		}
		switch status {
		case 200:
			if err != nil || resp == nil || string(resp.Body) != body {
				t.Fatalf("a success must survive a panicking observer: %v", err)
			}
		case 404:
			wantKind(t, err, NotFound)
		case 500:
			wantKind(t, err, Internal)
		}
	}
}

func TestObserve_ARetriedRequestIsObservedOncePerAttempt(t *testing.T) {
	tok := mustToken(t)
	handler := func(w http.ResponseWriter, r *http.Request, n int) {
		if n < 3 {
			jsonReply(w, 503, m1NotWired)
			return
		}
		jsonReply(w, 200, `{"ok":true}`)
	}
	s := newStub(t, handler)
	col := &collector{}
	cl := s.client(t, M2, tok, Options{Retries: 2, Sleep: (&recorder{}).sleep, Observe: col.observe})
	req := getReq("m2.report", "/v1/x")
	req.Idempotent = true
	if _, err := cl.Do(ctxT(t, 5*time.Second), req); err != nil {
		t.Fatal(err)
	}
	var statuses []int
	for _, o := range col.got() {
		statuses = append(statuses, o.Status)
	}
	if !reflect.DeepEqual(statuses, []int{503, 503, 200}) {
		t.Fatalf("observed statuses %v, want one per attempt: [503 503 200]", statuses)
	}
	// The paired control: a request that is not idempotent is not retried and so observed once.
	s2 := newStub(t, handler)
	col2 := &collector{}
	cl2 := s2.client(t, M2, tok, Options{Retries: 2, Sleep: (&recorder{}).sleep, Observe: col2.observe})
	_, err := cl2.Do(ctxT(t, 5*time.Second), getReq("m2.report", "/v1/x"))
	wantKind(t, err, Unavailable)
	if n := len(col2.got()); n != 1 {
		t.Fatalf("a request that is not retried is observed once, got %d", n)
	}
}

func TestObserve_TheBodyIsACopy(t *testing.T) {
	tok := mustToken(t)
	const body = `{"state":"completed"}`
	s := newStub(t, func(w http.ResponseWriter, r *http.Request, n int) { jsonReply(w, 200, body) })
	var kept []byte
	cl := s.client(t, M2, tok, Options{Observe: func(o Observation) {
		kept = o.Body
		for i := range o.Body { // the observer scribbles over its copy
			o.Body[i] = 'X'
		}
	}})
	resp, err := cl.Do(ctxT(t, 5*time.Second), getReq("m2.report", "/v1/x"))
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.Body) != body {
		t.Fatalf("the observer's writes reached the client's response: %q", resp.Body)
	}
	var v struct{ State string }
	if err := Decode(resp, &v, Object(Req("state", Str))); err != nil || v.State != "completed" {
		t.Fatalf("decoded %+v %v", v, err)
	}
	// And the other way round: the response's body is not what the observer holds.
	resp.Body[0] = 'Y'
	if kept[0] != 'X' {
		t.Fatal("the observer's copy aliases the client's")
	}
}

func TestObserve_NilIsFreeAndASetObserverCopiesTheBody(t *testing.T) {
	tok := mustToken(t)
	s := newStub(t, func(w http.ResponseWriter, r *http.Request, n int) { jsonReply(w, 200, `{}`) })
	cl := s.client(t, M2, tok, Options{})
	p := &prepared{route: "m2.report", method: "GET", path: "/v1/x"}
	raw := &rawResponse{status: 200, body: []byte(strings.Repeat("a", 4096))}
	if n := testing.AllocsPerRun(100, func() { cl.observe(p, raw) }); n != 0 {
		t.Fatalf("a nil observer must cost nothing, allocations = %v", n)
	}
	cl.opts.Observe = func(Observation) {}
	if n := testing.AllocsPerRun(100, func() { cl.observe(p, raw) }); n == 0 {
		t.Fatal("the control: a set observer receives a copy, which allocates")
	}
}

func TestObserve_AnOversizeBodyIsCappedAndMarkedTruncated(t *testing.T) {
	tok := mustToken(t)
	for _, c := range []struct {
		name      string
		body      string
		truncated bool
		kind      Kind
	}{
		{"one byte over the limit", strings.Repeat("a", 17), true, Contract},
		{"far over the limit", strings.Repeat("a", 5000), true, Contract},
		{"exactly at the limit", `{"k":"abcdefg"}`[:15] + " ", false, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newStub(t, func(w http.ResponseWriter, r *http.Request, n int) { jsonReply(w, 200, c.body) })
			col := &collector{}
			cl := s.client(t, M2, tok, Options{MaxBodyBytes: 16, Observe: col.observe})
			_, err := cl.Do(ctxT(t, 5*time.Second), getReq("m2.report", "/v1/x"))
			if c.kind != 0 {
				wantKind(t, err, c.kind)
			}
			got := col.got()
			if len(got) != 1 || got[0].Truncated != c.truncated || int64(len(got[0].Body)) > 16 {
				t.Fatalf("observation = %+v", got)
			}
			if c.truncated && string(got[0].Body) != c.body[:16] {
				t.Fatalf("the capped body must be the first 16 bytes, got %q", got[0].Body)
			}
		})
	}
}

func TestObserve_DefaultOptionsAndFromConfigLeaveItNil(t *testing.T) {
	if DefaultOptions().Observe != nil || FromConfig(config.Config{}).Observe != nil {
		t.Fatal("the observer is for tests: the usual options must not set one")
	}
	// The paired control: the field is real, a value set by a test survives New.
	tok := mustToken(t)
	called := false
	s := newStub(t, func(w http.ResponseWriter, r *http.Request, n int) { jsonReply(w, 200, `{}`) })
	opts := DefaultOptions()
	opts.Observe = func(Observation) { called = true }
	cl := s.client(t, M2, tok, opts)
	if _, err := cl.Do(ctxT(t, 5*time.Second), getReq("m2.report", "/v1/x")); err != nil || !called {
		t.Fatalf("called = %v, err = %v", called, err)
	}
}

func TestObserve_HealthAndReadinessAreObservedIncludingTheNotReadyDocument(t *testing.T) {
	tok := mustToken(t)
	s := newStub(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if r.URL.Path == "/healthz" {
			jsonReply(w, 200, healthOK)
			return
		}
		jsonReply(w, 503, senderReady503)
	})
	col := &collector{}
	cl := s.client(t, M2, tok, Options{Observe: col.observe})
	if _, err := cl.Health(ctxT(t, 5*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.Ready(ctxT(t, 5*time.Second)); err != nil {
		t.Fatalf("a not-ready document is an answer, not an error: %v", err)
	}
	got := col.got()
	if len(got) != 2 || got[0].Route != "m2.healthz" || got[0].Path != "/healthz" || got[1].Route != "m2.readyz" || got[1].Status != 503 || string(got[1].Body) != senderReady503 {
		t.Fatalf("observations = %+v", got)
	}
}

func TestObserve_RunsInTheCallersGoroutineBeforeDoReturns(t *testing.T) {
	tok := mustToken(t)
	s := newStub(t, func(w http.ResponseWriter, r *http.Request, n int) { jsonReply(w, 200, `{}`) })
	done := false
	cl := s.client(t, M2, tok, Options{Observe: func(Observation) {
		time.Sleep(30 * time.Millisecond)
		done = true
	}})
	if _, err := cl.Do(ctxT(t, 5*time.Second), getReq("m2.report", "/v1/x")); err != nil {
		t.Fatal(err)
	}
	if !done {
		t.Fatal("Do returned before the observer did: the hook must be synchronous")
	}
}

func TestObserve_ThePathIsAsSentWithoutQueryOrBaseAndTheQueryIsNotInAnyField(t *testing.T) {
	tok := mustToken(t)
	s := newStub(t, func(w http.ResponseWriter, r *http.Request, n int) { jsonReply(w, 200, `{}`) })
	col := &collector{}
	cl := s.client(t, M2, tok, Options{Observe: col.observe})
	req := getReq("m2.report", "/v1/jobs/"+url.PathEscape("a b/c"))
	req.Query = url.Values{"cursor": {"SECRETLOOKINGVALUE"}}
	if _, err := cl.Do(ctxT(t, 5*time.Second), req); err != nil {
		t.Fatal(err)
	}
	o := col.got()[0]
	if o.Path != "/v1/jobs/a%20b%2Fc" || strings.Contains(o.Path, "?") || strings.Contains(strings.Join([]string{o.Route, o.Method, o.Path}, " "), "SECRETLOOKINGVALUE") {
		t.Fatalf("observation = %+v", o)
	}
}

func TestObserve_ObservationsOfConcurrentCallsAreAllDelivered(t *testing.T) {
	tok := mustToken(t)
	s := newStub(t, func(w http.ResponseWriter, r *http.Request, n int) { jsonReply(w, 200, `{}`) })
	col := &collector{}
	cl := s.client(t, M2, tok, Options{Observe: col.observe})
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = cl.Do(ctxT(t, 5*time.Second), getReq("m2.report", "/v1/x"))
		}()
	}
	wg.Wait()
	if n := len(col.got()); n != 16 {
		t.Fatalf("observations = %d, want 16", n)
	}
}
