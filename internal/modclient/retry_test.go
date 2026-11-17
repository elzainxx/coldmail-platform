package modclient

import (
	"context"
	"errors"
	"net"
	"net/http"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestRetry_AnIdempotentGetRetriesThenReturnsTheLastError(t *testing.T) {
	tok := mustToken(t)
	get := func() Request {
		r := getReq("m2.report", "/v1/report")
		r.Idempotent = true
		return r
	}

	t.Run("it retries N times and the error says how many attempts it took", func(t *testing.T) {
		s := newStub(t, func(w http.ResponseWriter, r *http.Request, n int) { jsonReply(w, 503, m1NotWired) })
		rec := &recorder{}
		c := s.client(t, M2, tok, Options{Retries: 3, Backoff: 100 * time.Millisecond, Sleep: rec.sleep})
		resp, err := c.Do(ctxT(t, 5*time.Second), get())
		if resp != nil {
			t.Fatal("a value came back")
		}
		e := wantKind(t, err, Unavailable)
		if s.count() != 4 || e.Attempts != 4 || !strings.Contains(err.Error(), "4 attempts") {
			t.Fatalf("%d hit(s), %d attempt(s): %v", s.count(), e.Attempts, err)
		}
		if got, want := rec.got(), []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond}; !reflect.DeepEqual(got, want) {
			t.Fatalf("waits %v, want %v", got, want)
		}
	})

	t.Run("the error returned is the LAST one, not the first", func(t *testing.T) {
		s := newStub(t, func(w http.ResponseWriter, r *http.Request, n int) {
			switch n {
			case 1:
				jsonReply(w, 503, m1NotWired)
			case 2:
				jsonReply(w, 502, `bad gateway`)
			default:
				w.Header().Set("Retry-After", "1")
				jsonReply(w, 429, m3RateLimited)
			}
		})
		c := s.client(t, M2, tok, Options{Retries: 2, Sleep: (&recorder{}).sleep})
		_, err := c.Do(ctxT(t, 5*time.Second), get())
		e := wantKind(t, err, RateLimited)
		if e.Attempts != 3 || e.Code != "rate_limited" || e.RetryAfter != time.Second || s.count() != 3 {
			t.Fatalf("last error = %+v after %d hit(s)", *e, s.count())
		}

		// and when the last failure is a timeout, that is what comes back
		stall := newStub(t, func(w http.ResponseWriter, r *http.Request, n int) {
			if n < 3 {
				jsonReply(w, 503, m1NotWired)
				return
			}
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		})
		c2 := stall.client(t, M2, tok, Options{Retries: 2, Timeout: 150 * time.Millisecond, Sleep: (&recorder{}).sleep})
		_, err = c2.Do(ctxT(t, 10*time.Second), get())
		if e := wantKind(t, err, Timeout); e.Attempts != 3 {
			t.Fatalf("attempts %d", e.Attempts)
		}
	})

	t.Run("it stops retrying the moment the module answers, and returns the value", func(t *testing.T) {
		script := func(w http.ResponseWriter, r *http.Request, n int) {
			if n < 3 {
				jsonReply(w, 503, m1NotWired)
				return
			}
			jsonReply(w, 200, okBody)
		}
		s := newStub(t, script)
		rec := &recorder{}
		c := s.client(t, M2, tok, Options{Retries: 5, Backoff: 10 * time.Millisecond, Sleep: rec.sleep})
		resp, err := c.Do(ctxT(t, 5*time.Second), get())
		if err != nil || resp == nil || resp.Attempts != 3 || s.count() != 3 || len(rec.got()) != 2 {
			t.Fatalf("%+v, %v, %d hit(s), waits %v", resp, err, s.count(), rec.got())
		}
		// the paired control: with one retry fewer than it needs, the same script is an error
		s2 := newStub(t, script)
		c2 := s2.client(t, M2, tok, Options{Retries: 1, Sleep: (&recorder{}).sleep})
		resp, err = c2.Do(ctxT(t, 5*time.Second), get())
		if resp != nil || s2.count() != 2 {
			t.Fatalf("%+v after %d hit(s)", resp, s2.count())
		}
		wantKind(t, err, Unavailable)
	})

	t.Run("Retries 0 is no retry at all", func(t *testing.T) {
		s := newStub(t, func(w http.ResponseWriter, r *http.Request, n int) { jsonReply(w, 503, m1NotWired) })
		rec := &recorder{}
		c := s.client(t, M2, tok, Options{Retries: 0, Sleep: rec.sleep})
		_, err := c.Do(ctxT(t, 5*time.Second), get())
		if e := wantKind(t, err, Unavailable); s.count() != 1 || e.Attempts != 1 || len(rec.got()) != 0 {
			t.Fatalf("%d hit(s), %d attempt(s), waits %v", s.count(), e.Attempts, rec.got())
		}
	})

	t.Run("a refused connection is retried too, and says the request never left", func(t *testing.T) {
		// a listener that is closed at once: nothing accepts
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		_ = ln.Close()
		c, err := New(M2, "http://"+addr, "tok-x", transport(t), Options{Retries: 2, Timeout: time.Second, Sleep: (&recorder{}).sleep})
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.Do(ctxT(t, 10*time.Second), get())
		if e := wantKind(t, err, Unavailable); e.Attempts != 3 || e.Sent {
			t.Fatalf("%+v", *e)
		}
	})
}

func TestRetry_ANonIdempotentPostIsAttemptedExactlyOnce(t *testing.T) {
	tok := mustToken(t)
	modes := map[string]func(w http.ResponseWriter, r *http.Request, n int){
		"503 unavailable": func(w http.ResponseWriter, r *http.Request, n int) { jsonReply(w, 503, m1NotWired) },
		"429":             func(w http.ResponseWriter, r *http.Request, n int) { jsonReply(w, 429, m3RateLimited) },
		"a stall until the timeout": func(w http.ResponseWriter, r *http.Request, n int) {
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		},
	}
	for name, mk := range modes {
		t.Run(name, func(t *testing.T) {
			for _, idempotent := range []bool{false, true} {
				s := newStub(t, mk)
				rec := &recorder{}
				c := s.client(t, M1, tok, Options{Retries: 5, Timeout: 150 * time.Millisecond, Sleep: rec.sleep})
				req := Request{Route: "m1.submit", Method: http.MethodPost, Path: "/v1/verify/bulk", Body: map[string]any{"items": []string{}}, Idempotent: idempotent}
				resp, err := c.Do(ctxT(t, 20*time.Second), req)
				if resp != nil || err == nil {
					t.Fatalf("idempotent=%v: %+v, %v", idempotent, resp, err)
				}
				want, wantWaits := 1, 0
				if idempotent { // the paired control: the very same failures ARE retried once the caller says it is safe
					want, wantWaits = 6, 5
				}
				if s.count() != want || len(rec.got()) != wantWaits {
					t.Fatalf("idempotent=%v: %d attempt(s) and %d wait(s), want %d and %d", idempotent, s.count(), len(rec.got()), want, wantWaits)
				}
				if e := asErr(t, err); e.Attempts != want || !Retryable(err) {
					t.Fatalf("idempotent=%v: %+v", idempotent, *e)
				}
			}
		})
	}
}

func TestRetry_NeverAfterAnAnswerOfTheModule(t *testing.T) {
	tok := mustToken(t)
	rows := []struct {
		name   string
		status int
		ctype  string
		body   string
		kind   Kind
	}{
		{"400 refused", 400, "application/json", m2Validation, Refused},
		{"401 refused", 401, "application/json", m2Unauth, Refused},
		{"403 refused", 403, "application/json", adminForbidden, Refused},
		{"409 refused", 409, "application/json", m2Conflict, Refused},
		{"410 refused", 410, "", "", Refused},
		{"413 refused", 413, "application/json", m1TooLarge, Refused},
		{"422 refused", 422, "application/json", m3Unprocess, Refused},
		{"404 not found", 404, "application/json", m2NotFound, NotFound},
		{"404 bare", 404, "text/html", "<html>", NotFound},
		{"405 contract", 405, "application/json", m1WrongVerb, Contract},
		{"500 internal", 500, "application/json", m2Internal, Internal},
		{"501 internal", 501, "", "", Internal},
		{"503 paused", 503, "application/json", m2Paused, Paused},
		{"a body that is not JSON", 200, "application/json", "nope", Contract},
		{"the wrong content type", 200, "text/html", okBody, Contract},
		{"an unexpected 201", 201, "application/json", okBody, Contract},
		{"a redirect", 302, "", "", Contract},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			s := newStub(t, func(w http.ResponseWriter, r *http.Request, n int) {
				if row.ctype != "" {
					w.Header().Set("Content-Type", row.ctype)
				}
				w.WriteHeader(row.status)
				_, _ = w.Write([]byte(row.body))
			})
			rec := &recorder{}
			c := s.client(t, M2, tok, Options{Retries: 5, Sleep: rec.sleep})
			req := getReq("m2.report", "/v1/report")
			req.Idempotent = true
			_, err := c.Do(ctxT(t, 5*time.Second), req)
			e := wantKind(t, err, row.kind)
			if s.count() != 1 || e.Attempts != 1 || len(rec.got()) != 0 || Retryable(err) {
				t.Fatalf("%d attempt(s), waits %v, retryable %v", s.count(), rec.got(), Retryable(err))
			}
		})
	}
	// The paired control: a 503 `unavailable` on the same idempotent GET IS retried to the limit.
	s := newStub(t, func(w http.ResponseWriter, r *http.Request, n int) { jsonReply(w, 503, m1NotWired) })
	c := s.client(t, M2, tok, Options{Retries: 5, Sleep: (&recorder{}).sleep})
	req := getReq("m2.report", "/v1/report")
	req.Idempotent = true
	if _, err := c.Do(ctxT(t, 5*time.Second), req); err == nil || s.count() != 6 {
		t.Fatalf("control failed: %d attempt(s), %v", s.count(), err)
	}
}

func TestRetry_BackoffIsExponentialCappedAndJitterFree(t *testing.T) {
	tok := mustToken(t)
	run := func(backoff time.Duration, retries int) []time.Duration {
		s := newStub(t, func(w http.ResponseWriter, r *http.Request, n int) { jsonReply(w, 503, m1NotWired) })
		rec := &recorder{}
		c := s.client(t, M2, tok, Options{Retries: retries, Backoff: backoff, Sleep: rec.sleep})
		req := getReq("m2.report", "/v1/report")
		req.Idempotent = true
		if _, err := c.Do(ctxT(t, 5*time.Second), req); err == nil {
			t.Fatal("no error")
		}
		return rec.got()
	}
	ms := time.Millisecond
	if got, want := run(100*ms, 5), []time.Duration{100 * ms, 200 * ms, 400 * ms, 800 * ms, 1600 * ms}; !reflect.DeepEqual(got, want) {
		t.Errorf("waits %v, want %v", got, want)
	}
	if got, want := run(20*time.Second, 3), []time.Duration{20 * time.Second, 30 * time.Second, 30 * time.Second}; !reflect.DeepEqual(got, want) {
		t.Errorf("capped waits %v, want %v", got, want)
	}
	if got, want := run(30*time.Second, 2), []time.Duration{30 * time.Second, 30 * time.Second}; !reflect.DeepEqual(got, want) {
		t.Errorf("waits at the cap %v, want %v", got, want)
	}
	if got, want := run(0, 3), []time.Duration{0, 0, 0}; !reflect.DeepEqual(got, want) {
		t.Errorf("a zero backoff waited %v, want %v", got, want)
	}
	// Jitter-free: the same dials give the same waits, run after run.
	a, b := run(250*ms, 6), run(250*ms, 6)
	if !reflect.DeepEqual(a, b) || len(a) != 6 {
		t.Errorf("two runs differ (%v vs %v): there is jitter in the backoff", a, b)
	}
	// ten retries at the largest dial: every wait is within the cap
	for _, w := range run(30*time.Second, 10) {
		if w > MaxBackoff {
			t.Errorf("a wait of %s is over the cap", w)
		}
	}
}

func TestRetry_RetryAfterIsHonouredForRateLimitedAndOnlyThere(t *testing.T) {
	tok := mustToken(t)
	httpDate := time.Now().UTC().Add(10 * time.Second).Format(http.TimeFormat)
	rows := []struct {
		name       string
		status     int
		retryAfter string
		body       string
		backoff    time.Duration
		timeout    time.Duration
		reqTimeout time.Duration
		min, max   time.Duration
	}{
		{"seconds above the backoff", 429, "3", m3RateLimited, 250 * time.Millisecond, 15 * time.Second, 0, 3 * time.Second, 3 * time.Second},
		{"seconds below the backoff: the backoff wins", 429, "1", m3RateLimited, 2 * time.Second, 15 * time.Second, 0, 2 * time.Second, 2 * time.Second},
		{"seconds above the timeout: capped at the timeout", 429, "100", m3RateLimited, 250 * time.Millisecond, 5 * time.Second, 0, 5 * time.Second, 5 * time.Second},
		{"capped at the request's own timeout", 429, "100", m3RateLimited, 250 * time.Millisecond, 15 * time.Second, 2 * time.Second, 2 * time.Second, 2 * time.Second},
		{"an HTTP date", 429, httpDate, m3RateLimited, 250 * time.Millisecond, 15 * time.Second, 0, 8 * time.Second, 10 * time.Second},
		{"no header: the backoff", 429, "", m3RateLimited, 250 * time.Millisecond, 15 * time.Second, 0, 250 * time.Millisecond, 250 * time.Millisecond},
		{"an unreadable header: the backoff", 429, "soon", m3RateLimited, 250 * time.Millisecond, 15 * time.Second, 0, 250 * time.Millisecond, 250 * time.Millisecond},
		{"a 503 unavailable with Retry-After waits the backoff only", 503, "9", m1NotWired, 250 * time.Millisecond, 15 * time.Second, 0, 250 * time.Millisecond, 250 * time.Millisecond},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			s := newStub(t, func(w http.ResponseWriter, r *http.Request, n int) {
				if row.retryAfter != "" {
					w.Header().Set("Retry-After", row.retryAfter)
				}
				jsonReply(w, row.status, row.body)
			})
			rec := &recorder{}
			c := s.client(t, M3, tok, Options{Retries: 1, Backoff: row.backoff, Timeout: row.timeout, Sleep: rec.sleep})
			req := getReq("m3.report", "/v1/report")
			req.Idempotent = true
			req.Timeout = row.reqTimeout
			if _, err := c.Do(ctxT(t, 5*time.Second), req); err == nil {
				t.Fatal("no error")
			}
			w := rec.got()
			if len(w) != 1 || w[0] < row.min || w[0] > row.max {
				t.Fatalf("waited %v, want one wait in [%s, %s]", w, row.min, row.max)
			}
		})
	}
}

func TestRetry_TheContextEndsTheWholeCallAtOnce(t *testing.T) {
	tok := mustToken(t)
	always503 := func(w http.ResponseWriter, r *http.Request, n int) { jsonReply(w, 503, m1NotWired) }
	req := getReq("m2.report", "/v1/report")
	req.Idempotent = true

	t.Run("cancelled during the wait between attempts", func(t *testing.T) {
		s := newStub(t, always503)
		c := s.client(t, M2, tok, Options{Retries: 5, Backoff: 20 * time.Second}) // a real timer: the wait would be 20 s
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { _, err := c.Do(ctx, req); done <- err }()
		waitForHit(t, s)
		start := time.Now()
		cancel()
		select {
		case err := <-done:
			e := wantKind(t, err, Canceled)
			if !errors.Is(err, context.Canceled) || e.Attempts != 1 || s.count() != 1 || time.Since(start) > 2*time.Second {
				t.Fatalf("%+v, %d hit(s), %s after the cancel", *e, s.count(), time.Since(start))
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the call did not return after the context was cancelled: it is sleeping through the backoff")
		}
	})

	t.Run("cancelled before the call", func(t *testing.T) {
		s := newStub(t, func(w http.ResponseWriter, r *http.Request, n int) { jsonReply(w, 200, okBody) })
		c := s.client(t, M2, tok, Options{Retries: 5})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		resp, err := c.Do(ctx, req)
		if resp != nil {
			t.Fatal("a value came back")
		}
		if e := wantKind(t, err, Canceled); s.count() != 0 || e.Sent || e.Attempts != 0 {
			t.Fatalf("%+v, %d hit(s)", *e, s.count())
		}
		// the paired control: the same call under a live context reaches the module and returns its value
		if resp, err := c.Do(ctxT(t, 5*time.Second), req); err != nil || resp == nil || s.count() != 1 {
			t.Fatalf("control failed: %+v, %v, %d hit(s)", resp, err, s.count())
		}
	})

	t.Run("cancelled while an attempt is in flight", func(t *testing.T) {
		s := newStub(t, func(w http.ResponseWriter, r *http.Request, n int) { <-r.Context().Done() })
		c := s.client(t, M2, tok, Options{Retries: 5, Timeout: 30 * time.Second})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { _, err := c.Do(ctx, req); done <- err }()
		waitForHit(t, s)
		cancel()
		select {
		case err := <-done:
			if e := wantKind(t, err, Canceled); s.count() != 1 || !e.Sent {
				t.Fatalf("%+v, %d hit(s)", *e, s.count())
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the call did not return after the context was cancelled")
		}
	})

	t.Run("the caller's deadline passing during the wait is a Timeout", func(t *testing.T) {
		s := newStub(t, always503)
		c := s.client(t, M2, tok, Options{Retries: 5, Backoff: 20 * time.Second})
		start := time.Now()
		_, err := c.Do(ctxT(t, 300*time.Millisecond), req)
		if e := wantKind(t, err, Timeout); !errors.Is(err, context.DeadlineExceeded) || s.count() != 1 || e.Attempts != 1 || time.Since(start) > 5*time.Second {
			t.Fatalf("%+v, %d hit(s), %s", *e, s.count(), time.Since(start))
		}
	})
}

// waitForHit waits (at most ten seconds) until the stub has been called once.
func waitForHit(t *testing.T, s *stub) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for s.count() < 1 {
		if time.Now().After(deadline) {
			t.Fatal("the module was never called")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// settle polls until the goroutine count is at most limit, or the time is up, and returns the last count and whether it settled.
func settle(limit int, within time.Duration) (int, bool) {
	deadline := time.Now().Add(within)
	for {
		n := runtime.NumGoroutine()
		if n <= limit {
			return n, true
		}
		if time.Now().After(deadline) {
			return n, false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRetry_NoGoroutineOutlivesTheCalls(t *testing.T) {
	tok := mustToken(t)
	before := runtime.NumGoroutine()

	s := newStub(t, func(w http.ResponseWriter, r *http.Request, n int) {
		switch n % 6 {
		case 0:
			jsonReply(w, 200, okBody)
		case 1:
			jsonReply(w, 503, m1NotWired)
		case 2: // headers, then a stalled body
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"ok":`))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case 3: // nothing at all until the client gives up
			<-r.Context().Done()
		case 4:
			jsonReply(w, 429, m3RateLimited)
		default:
			jsonReply(w, 200, `{"pad":"`+strings.Repeat("x", 3000)+`"}`)
		}
	})
	c := s.client(t, M2, tok, Options{Retries: 2, Timeout: 120 * time.Millisecond, Backoff: time.Millisecond, MaxBodyBytes: 2048})
	for i := 0; i < 24; i++ {
		req := getReq("m2.report", "/v1/report")
		req.Idempotent = i%2 == 0
		_, _ = c.Do(ctxT(t, 10*time.Second), req)
	}
	// calls the context ends in the middle
	for i := 0; i < 4; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
		req := getReq("m2.report", "/v1/report")
		req.Idempotent = true
		_, _ = c.Do(ctx, req)
		cancel()
	}
	// refused connections
	dead, err := New(M2, "http://127.0.0.1:1", "tok-x", transport(t), Options{Retries: 2, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		r := getReq("m2.report", "/v1/report")
		r.Idempotent = true
		_, _ = dead.Do(ctxT(t, 5*time.Second), r)
	}
	s.srv.Close()
	c.CloseIdleConnections()
	dead.CloseIdleConnections()
	if n, ok := settle(before+1, 10*time.Second); !ok {
		buf := make([]byte, 1<<16)
		buf = buf[:runtime.Stack(buf, true)]
		t.Fatalf("%d goroutine(s) before the calls and %d after: something leaked\n%s", before, n, buf)
	}

	// The paired control: the same measurement DOES see a goroutine that is left running.
	base := runtime.NumGoroutine()
	stop := make(chan struct{})
	for i := 0; i < 50; i++ { // many, so that a few unrelated goroutines ending meanwhile cannot hide them
		go func() { <-stop }()
	}
	if _, ok := settle(base, 300*time.Millisecond); ok {
		t.Fatal("control failed: a leaked goroutine was not noticed, so the measurement above proves nothing")
	}
	close(stop)
	if _, ok := settle(base, 5*time.Second); !ok {
		t.Fatal("control failed: the measurement does not return to its baseline once the goroutine ends")
	}
}
