package modclient

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/config"
)

// okBody is what every table below answers with when its control flips the stub from failing to healthy.
const okBody = `{"ok":true}`

// failing is a stub that answers `bad` while fail is set and a healthy 200 otherwise: the paired control of every error test is the same route, answered correctly.
func failing(t testing.TB, bad func(w http.ResponseWriter, r *http.Request, n int)) (*stub, *atomic.Bool) {
	t.Helper()
	var fail atomic.Bool
	fail.Store(true)
	s := newStub(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if !fail.Load() {
			jsonReply(w, 200, okBody)
			return
		}
		bad(w, r, n)
	})
	return s, &fail
}

// control flips the stub to healthy and requires the same call to return the value.
func control(t *testing.T, c *Client, fail *atomic.Bool, req Request) {
	t.Helper()
	fail.Store(false)
	resp, err := c.Do(ctxT(t, 5*time.Second), req)
	if err != nil || resp == nil || string(resp.Body) != okBody {
		t.Fatalf("control failed: the same route answered correctly returned %+v, %v", resp, err)
	}
	fail.Store(true)
}

type statusRow struct {
	name       string
	status     int
	hdr        map[string]string
	ctype      string
	body       string
	kind       Kind
	code       string
	reqID      string
	enveloped  bool
	details    []Detail
	retryAfter time.Duration
}

func (r statusRow) write(w http.ResponseWriter) {
	for k, v := range r.hdr {
		w.Header().Set(k, v)
	}
	switch {
	case r.ctype != "":
		w.Header().Set("Content-Type", r.ctype)
	case strings.HasPrefix(strings.TrimSpace(r.body), "{") || strings.HasPrefix(strings.TrimSpace(r.body), "["):
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(r.status)
	_, _ = io.WriteString(w, r.body)
}

func TestKinds_EveryStatusAndEnvelopeMapsToItsKind(t *testing.T) {
	rows := []statusRow{
		// module 1's envelope (internal/api/errors.go)
		{name: "m1 unauthenticated", status: 401, body: m1Unauthenticated, kind: Refused, code: "unauthenticated", reqID: m1ID, enveloped: true},
		{name: "m1 validation_failed with details", status: 400, body: m1BadCursor, kind: Refused, code: "validation_failed", reqID: m1ID, enveloped: true, details: []Detail{{"cursor", "malformed"}}},
		{name: "m1 not_found", status: 404, body: m1NotFound, kind: NotFound, code: "not_found", reqID: m1ID, enveloped: true},
		{name: "m1 method_not_allowed", status: 405, body: m1WrongVerb, kind: Contract, code: "method_not_allowed", reqID: m1ID, enveloped: true},
		{name: "m1 payload_too_large", status: 413, body: m1TooLarge, kind: Refused, code: "payload_too_large", reqID: m1ID, enveloped: true, details: []Detail{{"items", "out_of_range"}}},
		{name: "m1 unavailable (not wired)", status: 503, body: m1NotWired, kind: Unavailable, code: "unavailable", reqID: m1ID, enveloped: true},
		{name: "m1 paused", status: 503, body: m1Paused, kind: Paused, code: "paused", reqID: m1ID, enveloped: true},
		{name: "m1 internal_error", status: 500, body: m1Internal, kind: Internal, code: "internal_error", reqID: m1ID, enveloped: true},
		{name: "m1 contract_violation", status: 500, body: m1ContractViolated, kind: Internal, code: "contract_violation", reqID: m1ID, enveloped: true},
		// modules 2 and 3 (internal/platformapi/errors.go) and the admin domains (internal/adminapi/errors.go)
		{name: "m2 validation_failed", status: 400, body: m2Validation, kind: Refused, code: "validation_failed", reqID: m2ID, enveloped: true, details: []Detail{{"user_id", "required"}}},
		{name: "m2 unauthenticated", status: 401, body: m2Unauth, kind: Refused, code: "unauthenticated", reqID: m2ID, enveloped: true},
		{name: "m2 not_found with details", status: 404, body: m2NotFound, kind: NotFound, code: "not_found", reqID: m2ID, enveloped: true, details: []Detail{{"job_item_id", "not_found"}}},
		{name: "m2 conflict", status: 409, body: m2Conflict, kind: Refused, code: "conflict", reqID: m2ID, enveloped: true},
		{name: "m2 paused", status: 503, body: m2Paused, kind: Paused, code: "paused", reqID: m2ID, enveloped: true},
		{name: "m2 internal_error", status: 500, body: m2Internal, kind: Internal, code: "internal_error", reqID: m2ID, enveloped: true},
		{name: "m3 unprocessable", status: 422, body: m3Unprocess, kind: Refused, code: "unprocessable", reqID: m2ID, enveloped: true, details: []Detail{{"paused", "conflict"}}},
		{name: "m3 suspended conflict", status: 409, body: m3Suspended, kind: Refused, code: "conflict", reqID: m2ID, enveloped: true},
		{name: "m3 rate_limited with Retry-After", status: 429, hdr: map[string]string{"Retry-After": "7"}, body: m3RateLimited, kind: RateLimited, code: "rate_limited", reqID: m2ID, enveloped: true, retryAfter: 7 * time.Second},
		{name: "admin forbidden", status: 403, body: adminForbidden, kind: Refused, code: "forbidden", reqID: m2ID, enveloped: true},
		{name: "admin config_ordering_violation", status: 400, body: adminOrdering, kind: Refused, code: "config_ordering_violation", reqID: m2ID, enveloped: true},
		// no envelope: the status alone decides
		{name: "bare 400", status: 400, kind: Refused},
		{name: "bare 401 text", status: 401, ctype: "text/plain", body: "Unauthorized", kind: Refused},
		{name: "bare 403 html", status: 403, ctype: "text/html", body: "<html>no</html>", kind: Refused},
		{name: "bare 409", status: 409, kind: Refused},
		{name: "bare 410", status: 410, kind: Refused},
		{name: "bare 413", status: 413, kind: Refused},
		{name: "bare 422 html", status: 422, ctype: "text/html", body: "<html>no</html>", kind: Refused},
		{name: "bare 404", status: 404, kind: NotFound},
		{name: "bare 404 html", status: 404, ctype: "text/html", body: "<html>404</html>", kind: NotFound},
		{name: "bare 429", status: 429, kind: RateLimited},
		{name: "bare 408", status: 408, kind: Timeout},
		{name: "bare 500 html", status: 500, ctype: "text/html", body: "<html>oops</html>", kind: Internal},
		{name: "501", status: 501, kind: Internal},
		{name: "507", status: 507, kind: Internal},
		{name: "bare 502 html", status: 502, ctype: "text/html", body: "<html>bad gateway</html>", kind: Unavailable},
		{name: "bare 503", status: 503, kind: Unavailable},
		{name: "bare 504", status: 504, kind: Unavailable},
		// an additive change: a code this client has never seen degrades by status, and is kept as data
		{name: "a new code on a 409", status: 409, body: `{"error":{"code":"stale_version","message":"x","request_id":"` + m2ID + `"}}`, kind: Refused, code: "stale_version", reqID: m2ID, enveloped: true},
		{name: "a new code on a 503", status: 503, body: `{"error":{"code":"warming_up","message":"x","request_id":"` + m2ID + `"}}`, kind: Unavailable, code: "warming_up", reqID: m2ID, enveloped: true},
		{name: "a new code on a 500", status: 500, body: `{"error":{"code":"disk_full","message":"x","request_id":"` + m2ID + `"}}`, kind: Internal, code: "disk_full", reqID: m2ID, enveloped: true},
		// the status decides when the code disagrees with it
		{name: "paused on a 500", status: 500, body: m2Paused, kind: Internal, code: "paused", reqID: m2ID, enveloped: true},
		{name: "conflict on a 404", status: 404, body: m2Conflict, kind: NotFound, code: "conflict", reqID: m2ID, enveloped: true},
		{name: "not_found on a 503", status: 503, body: m2NotFound, kind: Unavailable, code: "not_found", reqID: m2ID, enveloped: true, details: []Detail{{"job_item_id", "not_found"}}},
		// a status the route does not use
		{name: "a status 418", status: 418, kind: Contract},
		{name: "a 201 where 200 is expected", status: 201, ctype: "application/json", body: `{}`, kind: Contract},
		{name: "a 204 where 200 is expected", status: 204, kind: Contract},
		{name: "a redirect", status: 307, hdr: map[string]string{"Location": "/elsewhere"}, kind: Contract},
	}
	if len(rows) < 45 {
		t.Fatalf("the table shrank to %d rows", len(rows))
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			tok := mustToken(t)
			s, fail := failing(t, func(w http.ResponseWriter, r *http.Request, n int) { row.write(w) })
			c := s.client(t, M2, tok, Options{})
			req := getReq("m2.report", "/v1/report")

			resp, err := c.Do(ctxT(t, 5*time.Second), req)
			if resp != nil {
				t.Fatalf("a value came back with an error: %+v", resp)
			}
			e := wantKind(t, err, row.kind)
			if e.Status != row.status || e.Code != row.code || e.RequestID != row.reqID || e.Enveloped != row.enveloped || e.Module != M2 || e.Route != "m2.report" ||
				e.RetryAfter != row.retryAfter || !e.Sent || e.Attempts != 1 {
				t.Fatalf("error facts = %+v", *e)
			}
			if !(len(e.Details) == 0 && len(row.details) == 0) && !reflect.DeepEqual(e.Details, row.details) {
				t.Fatalf("details = %v, want %v", e.Details, row.details)
			}
			// nothing from the response body reaches the error text
			if text := err.Error(); strings.Contains(text, "oops") || strings.Contains(text, "<html>") || strings.Contains(text, "retry later") || strings.Contains(text, "fixed sentence") {
				t.Fatalf("the error carries response text: %s", text)
			}
			control(t, c, fail, req)
		})
	}
}

func TestKinds_TransportFailuresAreUnavailable(t *testing.T) {
	ok := func(t *testing.T, c *Client) {
		t.Helper()
		resp, err := c.Do(ctxT(t, 5*time.Second), getReq("m2.report", "/v1/report"))
		if err != nil || resp == nil || string(resp.Body) != okBody {
			t.Fatalf("control failed: %+v, %v", resp, err)
		}
	}
	tok := mustToken(t)

	t.Run("connection refused", func(t *testing.T) {
		s := newStub(t, func(w http.ResponseWriter, r *http.Request, n int) { jsonReply(w, 200, okBody) })
		c := s.client(t, M2, tok, Options{})
		ok(t, c) // control: the server is there
		s.srv.Close()
		c.CloseIdleConnections()
		resp, err := c.Do(ctxT(t, 5*time.Second), getReq("m2.report", "/v1/report"))
		if resp != nil {
			t.Fatal("a value came back")
		}
		e := wantKind(t, err, Unavailable)
		if e.Sent || e.Status != 0 {
			t.Fatalf("a refused connection reached the module: %+v", *e)
		}
	})

	t.Run("dns failure", func(t *testing.T) {
		fail := true
		rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if fail {
				return nil, &net.DNSError{Err: "no such host", Name: "module.invalid", IsNotFound: true}
			}
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(okBody)), Request: r}, nil
		})
		c, err := New(M2, "http://module.invalid:8080", config.Secret(tok), &http.Client{Transport: rt}, Options{})
		if err != nil {
			t.Fatal(err)
		}
		resp, err := c.Do(ctxT(t, 5*time.Second), getReq("m2.report", "/v1/report"))
		if resp != nil {
			t.Fatal("a value came back")
		}
		e := wantKind(t, err, Unavailable)
		var dns *net.DNSError
		if e.Sent || !errors.As(err, &dns) || !dns.IsNotFound || strings.Contains(err.Error(), "module.invalid") {
			t.Fatalf("a DNS failure is not recognised or leaks the name: %+v / %v", *e, err)
		}
		fail = false
		ok(t, c)
	})

	t.Run("connection reset", func(t *testing.T) {
		s, fail := failing(t, func(w http.ResponseWriter, r *http.Request, n int) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			if tcp, isTCP := conn.(*net.TCPConn); isTCP {
				_ = tcp.SetLinger(0) // close with a RST, not a FIN
			}
			_ = conn.Close()
		})
		c := s.client(t, M2, tok, Options{})
		resp, err := c.Do(ctxT(t, 5*time.Second), getReq("m2.report", "/v1/report"))
		if resp != nil {
			t.Fatal("a value came back")
		}
		if e := wantKind(t, err, Unavailable); !e.Sent {
			t.Fatalf("a reset after the request was written must read as possibly sent: %+v", *e)
		}
		control(t, c, fail, getReq("m2.report", "/v1/report"))
	})

	t.Run("connection closed without a response", func(t *testing.T) {
		s, fail := failing(t, func(w http.ResponseWriter, r *http.Request, n int) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
		})
		c := s.client(t, M2, tok, Options{})
		resp, err := c.Do(ctxT(t, 5*time.Second), getReq("m2.report", "/v1/report"))
		if resp != nil {
			t.Fatal("a value came back")
		}
		wantKind(t, err, Unavailable)
		control(t, c, fail, getReq("m2.report", "/v1/report"))
	})

	t.Run("body cut short of its declared length", func(t *testing.T) {
		s, fail := failing(t, func(w http.ResponseWriter, r *http.Request, n int) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", "100")
			w.WriteHeader(200)
			_, _ = io.WriteString(w, `{"ok":`) // 6 of the 100 bytes it promised
		})
		c := s.client(t, M2, tok, Options{})
		resp, err := c.Do(ctxT(t, 5*time.Second), getReq("m2.report", "/v1/report"))
		if resp != nil {
			t.Fatal("a half-read body came back as a value")
		}
		wantKind(t, err, Unavailable)
		control(t, c, fail, getReq("m2.report", "/v1/report"))
	})

	t.Run("a certificate the client does not trust", func(t *testing.T) {
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { jsonReply(w, 200, okBody) }))
		srv.Config.ErrorLog = log.New(io.Discard, "", 0) // the refused handshake is expected: keep it out of the test log
		srv.StartTLS()
		t.Cleanup(srv.Close)
		untrusting, err := New(M2, srv.URL, config.Secret(tok), transport(t), Options{})
		if err != nil {
			t.Fatal(err)
		}
		resp, err := untrusting.Do(ctxT(t, 5*time.Second), getReq("m2.report", "/v1/report"))
		if resp != nil {
			t.Fatal("a value came back")
		}
		if e := wantKind(t, err, Unavailable); e.Sent {
			t.Fatalf("a failed handshake cannot have delivered the request: %+v", *e)
		}
		// the paired control: a client that trusts the server's certificate gets the value
		tr := srv.Client().Transport.(*http.Transport).Clone()
		tr.TLSClientConfig = &tls.Config{RootCAs: srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs}
		t.Cleanup(tr.CloseIdleConnections)
		trusting, err := New(M2, srv.URL, config.Secret(tok), &http.Client{Transport: tr}, Options{})
		if err != nil {
			t.Fatal(err)
		}
		ok(t, trusting)
	})
}

func TestKinds_TimeoutsAreTimeouts(t *testing.T) {
	tok := mustToken(t)
	slow := func(s *stub, hold time.Duration) func(w http.ResponseWriter, r *http.Request, n int) {
		return func(w http.ResponseWriter, r *http.Request, n int) {
			select {
			case <-time.After(hold):
			case <-r.Context().Done():
				return
			case <-s.release:
				return
			}
			jsonReply(w, 200, okBody)
		}
	}
	t.Run("no response within the request timeout", func(t *testing.T) {
		var s *stub
		s = newStub(t, func(w http.ResponseWriter, r *http.Request, n int) { slow(s, 700*time.Millisecond)(w, r, n) })
		c := s.client(t, M2, tok, Options{Timeout: 200 * time.Millisecond})
		start := time.Now()
		resp, err := c.Do(ctxT(t, 10*time.Second), getReq("m2.report", "/v1/report"))
		if resp != nil {
			t.Fatal("a value came back")
		}
		e := wantKind(t, err, Timeout)
		if !errors.Is(err, context.DeadlineExceeded) || e.Status != 0 || !e.Sent || time.Since(start) > 4*time.Second {
			t.Fatalf("timeout error = %+v after %s", *e, time.Since(start))
		}
		// the paired control: the same module, answering in the same 700 ms, is served by a client whose timeout allows it
		patient := s.client(t, M2, tok, Options{Timeout: 5 * time.Second})
		if resp, err := patient.Do(ctxT(t, 10*time.Second), getReq("m2.report", "/v1/report")); err != nil || resp == nil || string(resp.Body) != okBody {
			t.Fatalf("control failed: %+v, %v", resp, err)
		}
	})

	t.Run("headers arrive and the body stalls", func(t *testing.T) {
		stall := true
		s := newStub(t, func(w http.ResponseWriter, r *http.Request, n int) {
			w.Header().Set("Content-Type", "application/json")
			if !stall {
				jsonReply(w, 200, okBody)
				return
			}
			w.WriteHeader(200)
			_, _ = io.WriteString(w, `{"ok":`)
			w.(http.Flusher).Flush()
			select { // the rest never comes
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		})
		c := s.client(t, M2, tok, Options{Timeout: 300 * time.Millisecond})
		start := time.Now()
		resp, err := c.Do(ctxT(t, 10*time.Second), getReq("m2.report", "/v1/report"))
		if resp != nil {
			t.Fatal("half a document came back as a value")
		}
		wantKind(t, err, Timeout)
		if time.Since(start) > 4*time.Second {
			t.Fatalf("a stalled body held the call for %s", time.Since(start))
		}
		stall = false
		if resp, err := c.Do(ctxT(t, 5*time.Second), getReq("m2.report", "/v1/report")); err != nil || string(resp.Body) != okBody {
			t.Fatalf("control failed: %v", err)
		}
	})

	t.Run("a per-request timeout replaces the dial", func(t *testing.T) {
		var s *stub
		s = newStub(t, func(w http.ResponseWriter, r *http.Request, n int) { slow(s, 400*time.Millisecond)(w, r, n) })
		c := s.client(t, M2, tok, Options{Timeout: 100 * time.Millisecond})
		_, err := c.Do(ctxT(t, 10*time.Second), getReq("m2.report", "/v1/report"))
		wantKind(t, err, Timeout) // the dial alone is too short for this module
		longer := getReq("m2.report", "/v1/report")
		longer.Timeout = 5 * time.Second
		resp, err := c.Do(ctxT(t, 10*time.Second), longer)
		if err != nil || resp == nil || string(resp.Body) != okBody {
			t.Fatalf("the per-request timeout did not apply: %+v, %v", resp, err)
		}
	})

	t.Run("the caller's own deadline", func(t *testing.T) {
		var s *stub
		s = newStub(t, func(w http.ResponseWriter, r *http.Request, n int) { slow(s, 700*time.Millisecond)(w, r, n) })
		c := s.client(t, M2, tok, Options{Timeout: 5 * time.Second, Retries: 3, Backoff: time.Millisecond})
		start := time.Now()
		req := getReq("m2.report", "/v1/report")
		req.Idempotent = true
		resp, err := c.Do(ctxT(t, 200*time.Millisecond), req)
		if resp != nil {
			t.Fatal("a value came back")
		}
		e := wantKind(t, err, Timeout)
		if !errors.Is(err, context.DeadlineExceeded) || s.count() != 1 || time.Since(start) > 4*time.Second {
			t.Fatalf("the caller's deadline did not end the call: %+v, %d hit(s), %s", *e, s.count(), time.Since(start))
		}
		// the paired control: the same call under a context that allows the module its 700 ms is served
		if resp, err := c.Do(ctxT(t, 10*time.Second), req); err != nil || resp == nil || string(resp.Body) != okBody {
			t.Fatalf("control failed: %+v, %v", resp, err)
		}
	})
}

func TestKinds_AnAnswerThatBreaksTheContractIsAContractError(t *testing.T) {
	tok := mustToken(t)
	big := `{"pad":"` + strings.Repeat("x", 2000) + `"}`
	rows := []struct {
		name   string
		ctype  string
		status int
		body   string
		limit  int64
		expect []int
		want   string // a phrase of the message
	}{
		{"a body that is not JSON", "application/json", 200, "hello, I am a proxy", 0, nil, "not JSON"},
		{"valid JSON served as text/html", "text/html", 200, `{"ok":true}`, 0, nil, "application/json"},
		{"valid JSON served as text/plain", "text/plain; charset=utf-8", 200, `{"ok":true}`, 0, nil, "application/json"},
		{"no content type at all", "", 200, `{"ok":true}`, 0, nil, "application/json"},
		{"a document cut off in the middle", "application/json", 200, `{"ok":tr`, 0, nil, "truncated"},
		{"a document cut off after a key", "application/json", 200, `{"results":[{"a":1},`, 0, nil, "truncated"},
		{"two documents", "application/json", 200, `{"ok":true} {"ok":false}`, 0, nil, "data after"},
		{"data after the document", "application/json", 200, `{"ok":true}garbage`, 0, nil, "data after"},
		{"a body over the size limit", "application/json", 200, big, 1024, nil, "larger than the limit"},
		{"a 201 where only 200 is expected", "application/json", 201, `{"ok":true}`, 0, nil, "does not answer with HTTP 201"},
		{"a 202 where only 200 is expected", "application/json", 202, `{"ok":true}`, 0, nil, "does not answer with HTTP 202"},
		{"a 204 where only 200 is expected", "", 204, "", 0, nil, "does not answer with HTTP 204"},
		{"a 200 where only 202 is expected", "application/json", 200, `{"ok":true}`, 0, []int{202}, "does not answer with HTTP 200"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			s, fail := failing(t, func(w http.ResponseWriter, r *http.Request, n int) {
				if row.ctype != "" {
					w.Header().Set("Content-Type", row.ctype)
				} else {
					w.Header()["Content-Type"] = nil // no header at all, and no sniffing
				}
				w.WriteHeader(row.status)
				_, _ = io.WriteString(w, row.body)
			})
			c := s.client(t, M2, tok, Options{Retries: 3, MaxBodyBytes: row.limit})
			req := getReq("m2.report", "/v1/report")
			req.Idempotent = true // a Contract error is still never retried
			req.Expect = row.expect
			resp, err := c.Do(ctxT(t, 5*time.Second), req)
			if resp != nil {
				t.Fatalf("a value came back: %+v", resp)
			}
			e := wantKind(t, err, Contract)
			if !strings.Contains(e.Message, row.want) || e.Status != row.status || !e.Sent || e.Attempts != 1 || s.count() != 1 {
				t.Fatalf("error = %+v after %d hit(s), want a message containing %q", *e, s.count(), row.want)
			}
			if strings.Contains(err.Error(), "proxy") || strings.Contains(err.Error(), "garbage") {
				t.Fatalf("the error carries response text: %v", err)
			}
			if row.expect == nil {
				control(t, c, fail, req)
			}
		})
	}

	t.Run("the size limit is exact", func(t *testing.T) {
		body := `{"pad":"` + strings.Repeat("y", 100) + `"}`
		s := newStub(t, func(w http.ResponseWriter, r *http.Request, n int) { jsonReply(w, 200, body) })
		at := s.client(t, M2, tok, Options{MaxBodyBytes: int64(len(body))})
		if resp, err := at.Do(ctxT(t, 5*time.Second), getReq("m2.report", "/v1/report")); err != nil || len(resp.Body) != len(body) {
			t.Fatalf("a body of exactly the limit was refused: %v", err)
		}
		under := s.client(t, M2, tok, Options{MaxBodyBytes: int64(len(body)) - 1})
		_, err := under.Do(ctxT(t, 5*time.Second), getReq("m2.report", "/v1/report"))
		wantKind(t, err, Contract)
	})

	t.Run("a huge error body is not read past the limit and does not change the kind", func(t *testing.T) {
		var written atomic.Int64
		s := newStub(t, func(w http.ResponseWriter, r *http.Request, n int) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(500)
			chunk := []byte(strings.Repeat("z", 64<<10))
			for i := 0; i < 1024; i++ { // up to 64 MiB, if the client keeps reading
				k, err := w.Write(chunk)
				written.Add(int64(k))
				if err != nil {
					return
				}
			}
		})
		c := s.client(t, M2, tok, Options{MaxBodyBytes: 4096})
		_, err := c.Do(ctxT(t, 10*time.Second), getReq("m2.report", "/v1/report"))
		wantKind(t, err, Internal)
		if written.Load() > 32<<20 {
			t.Fatalf("the server wrote %d bytes: the client kept reading past its limit", written.Load())
		}
	})

	t.Run("an empty 204 that the route expects is a success", func(t *testing.T) {
		s := newStub(t, func(w http.ResponseWriter, r *http.Request, n int) { w.WriteHeader(204) })
		c := s.client(t, M3, tok, Options{})
		resp, err := c.Do(ctxT(t, 5*time.Second), Request{Route: "m3.accounts.delete", Method: http.MethodDelete, Path: "/v1/send-byo/accounts/a", Expect: []int{204}})
		if err != nil || resp == nil || resp.Status != 204 || len(resp.Body) != 0 {
			t.Fatalf("a 204 the route expects was refused: %+v, %v", resp, err)
		}
	})

	t.Run("a route that answers 200 or 202 accepts both", func(t *testing.T) {
		s := newStub(t, func(w http.ResponseWriter, r *http.Request, n int) {
			if n == 1 {
				jsonReply(w, 202, `{"job_id":"j","state":"queued"}`)
				return
			}
			jsonReply(w, 200, `{"job_id":"j","state":"sent"}`)
		})
		c := s.client(t, M2, tok, Options{})
		req := Request{Route: "m2.run", Method: http.MethodPost, Path: "/v1/run", Body: map[string]string{"job_item_id": "ji"}, Expect: []int{200, 202}}
		for _, want := range []int{202, 200} {
			if resp, err := c.Do(ctxT(t, 5*time.Second), req); err != nil || resp.Status != want {
				t.Fatalf("status %d was not accepted: %+v, %v", want, resp, err)
			}
		}
	})
}

func TestKinds_AnErrorBodyThatClaimsToBeAnEnvelopeAndIsNotIsAContractErrorOnA4xxOnly(t *testing.T) {
	tok := mustToken(t)
	rows := []struct {
		name      string
		status    int
		body      string
		kind      Kind
		enveloped bool
	}{
		{"error is a string on a 400", 400, `{"error":"bad input"}`, Contract, false},
		{"error is a string on a 404", 404, `{"error":"not found"}`, Contract, false},
		{"error is null", 400, `{"error":null}`, Contract, false},
		{"no code", 409, `{"error":{"message":"x"}}`, Contract, false},
		{"a code that is not a string", 409, `{"error":{"code":5}}`, Contract, false},
		{"a code outside the safe alphabet", 422, `{"error":{"code":"UPPER CASE","message":"x"}}`, Contract, false},
		{"details that is not an array", 400, `{"error":{"code":"validation_failed","details":"nope"}}`, Contract, false},
		{"a detail without an issue", 400, `{"error":{"code":"validation_failed","details":[{"field":"a"}]}}`, Contract, false},
		{"a malformed envelope on a 502 (a gateway, not the module)", 502, `{"error":"upstream"}`, Unavailable, false},
		{"a malformed envelope on a 503", 503, `{"error":"x"}`, Unavailable, false},
		{"a malformed envelope on a 500", 500, `{"error":"x"}`, Internal, false},
		{"a malformed envelope on a 408", 408, `{"error":"x"}`, Timeout, false},
		{"an object without an error key", 404, `{"detail":"nope"}`, NotFound, false},
		{"a JSON array", 401, `[]`, Refused, false},
		{"a JSON string", 401, `"x"`, Refused, false},
		{"JSON null", 401, `null`, Refused, false},
		{"an envelope without a request id", 409, `{"error":{"code":"conflict"}}`, Refused, true},
		{"an envelope with trailing data", 409, `{"error":{"code":"conflict"}} x`, Refused, false},
		{"a well-formed envelope (the control)", 409, m2Conflict, Refused, true},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			s := newStub(t, func(w http.ResponseWriter, r *http.Request, n int) { jsonReply(w, row.status, row.body) })
			c := s.client(t, M2, tok, Options{})
			resp, err := c.Do(ctxT(t, 5*time.Second), getReq("m2.report", "/v1/report"))
			if resp != nil {
				t.Fatal("a value came back")
			}
			e := wantKind(t, err, row.kind)
			if e.Enveloped != row.enveloped {
				t.Fatalf("Enveloped = %v, want %v: %+v", e.Enveloped, row.enveloped, *e)
			}
		})
	}
}

func TestRetryAfter_IsDataOnTheError(t *testing.T) {
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		header string
		want   time.Duration
	}{
		{"7", 7 * time.Second}, {" 12 ", 12 * time.Second}, {"0", 0}, {"-5", 0}, {"", 0}, {"soon", 0}, {"1.5", 0},
		{"99999999999", 24 * time.Hour},
		{"Tue, 17 Nov 2026 10:01:30 GMT", 90 * time.Second},
		{"Tue, 17 Nov 2026 09:59:00 GMT", 0},
		{"Fri, 20 Nov 2026 10:00:00 GMT", 24 * time.Hour},
	} {
		h := http.Header{}
		if c.header != "" {
			h.Set("Retry-After", c.header)
		}
		if got := parseRetryAfter(h, now); got != c.want {
			t.Errorf("Retry-After %q = %s, want %s", c.header, got, c.want)
		}
	}

	tok := mustToken(t)
	s := newStub(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if n == 1 {
			w.Header().Set("Retry-After", "3")
			jsonReply(w, 429, m3RateLimited)
			return
		}
		if n == 2 {
			w.Header().Set("Retry-After", "5")
			jsonReply(w, 503, m2Paused)
			return
		}
		jsonReply(w, 429, m3RateLimited) // no Retry-After
	})
	rec := &recorder{}
	c := s.client(t, M3, tok, Options{Sleep: rec.sleep})
	_, err := c.Do(ctxT(t, 5*time.Second), getReq("m3.report", "/v1/report"))
	if e := wantKind(t, err, RateLimited); e.RetryAfter != 3*time.Second {
		t.Fatalf("RetryAfter = %s, want 3s", e.RetryAfter)
	}
	_, err = c.Do(ctxT(t, 5*time.Second), getReq("m3.report", "/v1/report"))
	if e := wantKind(t, err, Paused); e.RetryAfter != 5*time.Second {
		t.Fatalf("a 503's Retry-After is data too: %s", e.RetryAfter)
	}
	_, err = c.Do(ctxT(t, 5*time.Second), getReq("m3.report", "/v1/report"))
	if e := wantKind(t, err, RateLimited); e.RetryAfter != 0 {
		t.Fatalf("RetryAfter = %s without a header", e.RetryAfter)
	}
	if w := rec.got(); len(w) != 0 {
		t.Fatalf("the client slept %v: Retry-After is data on the error, not a sleep (the call was not idempotent)", w)
	}
}

func TestRequestID_RidesOnEveryError(t *testing.T) {
	tok := mustToken(t)
	do := func(t *testing.T, status int, hdr, body string) (*Response, error) {
		t.Helper()
		s := newStub(t, func(w http.ResponseWriter, r *http.Request, n int) {
			if hdr != "" {
				w.Header().Set("X-Request-Id", hdr)
			}
			jsonReply(w, status, body)
		})
		return s.client(t, M2, tok, Options{}).Do(ctxT(t, 5*time.Second), getReq("m2.report", "/v1/report"))
	}
	t.Run("the envelope's id wins over a header", func(t *testing.T) {
		_, err := do(t, 409, "hdr-id-1", m2Conflict)
		if e := wantKind(t, err, Refused); e.RequestID != m2ID {
			t.Fatalf("RequestID %q, want the envelope's", e.RequestID)
		}
	})
	t.Run("a header is used when the envelope has no id or there is no envelope", func(t *testing.T) {
		_, err := do(t, 409, "hdr-id-1", `{"error":{"code":"conflict"}}`)
		if e := wantKind(t, err, Refused); e.RequestID != "hdr-id-1" {
			t.Fatalf("RequestID %q", e.RequestID)
		}
		_, err = do(t, 500, "hdr-id-2", `oops`)
		if e := wantKind(t, err, Internal); e.RequestID != "hdr-id-2" {
			t.Fatalf("RequestID %q", e.RequestID)
		}
	})
	t.Run("an id outside the safe alphabet or too long is dropped", func(t *testing.T) {
		for _, bad := range []string{"has space", "semi;colon", strings.Repeat("a", 65), "tab\there", "ünï"} {
			_, err := do(t, 500, bad, `oops`)
			if e := wantKind(t, err, Internal); e.RequestID != "" {
				t.Fatalf("RequestID %q kept from %q", e.RequestID, bad)
			}
		}
		// the control: the same header, well formed, is kept
		_, err := do(t, 500, strings.Repeat("a", 64), `oops`)
		if e := wantKind(t, err, Internal); e.RequestID != strings.Repeat("a", 64) {
			t.Fatalf("a 64-byte id was dropped: %q", e.RequestID)
		}
	})
	t.Run("a success carries an id from a header or from the body, and otherwise none", func(t *testing.T) {
		resp, err := do(t, 200, "hdr-ok", `{"ok":true}`)
		if err != nil || resp.RequestID != "hdr-ok" {
			t.Fatalf("%+v, %v", resp, err)
		}
		resp, err = do(t, 200, "", `{"request_id":"`+m2ID+`","results":[]}`)
		if err != nil || resp.RequestID != m2ID {
			t.Fatalf("%+v, %v", resp, err)
		}
		resp, err = do(t, 200, "", `{"request_id":{"a":1}}`)
		if err != nil || resp.RequestID != "" {
			t.Fatalf("%+v, %v", resp, err)
		}
		resp, err = do(t, 200, "", `[1,2]`)
		if err != nil || resp.RequestID != "" {
			t.Fatalf("%+v, %v", resp, err)
		}
	})
}

func TestErrorSentinels_AreKindExactAndRetryableMeansWhatTheClientRetries(t *testing.T) {
	kinds := []Kind{Unavailable, Paused, Timeout, Internal, Contract, Refused, NotFound, RateLimited, Canceled, Invalid, NotSupported}
	if len(sentinels) != len(kinds) {
		t.Fatalf("%d sentinels for %d kinds", len(sentinels), len(kinds))
	}
	names := map[string]bool{}
	for _, k := range kinds {
		e := &Error{Kind: k, Module: M2, Route: "m2.x", Message: "m"}
		wrapped := fmt.Errorf("adapter: %w", fmt.Errorf("layer: %w", e))
		wantKind(t, wrapped, k) // errors.Is true for its own sentinel only, through two wrappers
		if KindOf(wrapped) != k {
			t.Errorf("KindOf(%s) = %s", k, KindOf(wrapped))
		}
		if names[k.String()] || k.String() == "" || strings.HasPrefix(k.String(), "unknown") {
			t.Errorf("kind %d has the name %q", k, k.String())
		}
		names[k.String()] = true
		wantRetry := k == Unavailable || k == Timeout || k == RateLimited
		if Retryable(wrapped) != wantRetry {
			t.Errorf("Retryable(%s) = %v", k, Retryable(wrapped))
		}
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%d", "%x"} {
			if got := fmt.Sprintf(verb, e); got != e.Error() {
				t.Errorf("%s of an Error prints %q, want its Error() %q", verb, got, e.Error())
			}
		}
	}
	// what is not an *Error
	plain := errors.New("plain")
	if KindOf(plain) != 0 || Retryable(plain) || Retryable(nil) || KindOf(nil) != 0 || errors.Is(plain, ErrUnavailable) {
		t.Error("a plain error was taken for a module error")
	}
	if Kind(0).String() == "" || !strings.HasPrefix(Kind(0).String(), "unknown") {
		t.Errorf("the zero Kind is named %q", Kind(0).String())
	}
	var nilErr *Error
	if nilErr.Error() == "" || nilErr.Unwrap() != nil || nilErr.Is(ErrUnavailable) {
		t.Error("a nil *Error is not safe to use")
	}
	// A Cause is reachable by errors.Is and errors.As and prints as a class word.
	c := &Error{Kind: Timeout, Cause: cause("the request timed out", context.DeadlineExceeded)}
	if !errors.Is(c, context.DeadlineExceeded) || errors.Unwrap(errors.Unwrap(c)) != nil {
		t.Error("a cause is not reachable by errors.Is, or it can be unwrapped to its raw text")
	}
}
