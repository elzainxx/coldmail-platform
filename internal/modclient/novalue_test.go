package modclient

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

// NEVER INVENTS AN OUTCOME. Every method returns (value, error); on any error the value must be nil. This file proves it over every operation and every way a call can fail, and
// proves that the proof can fail: the same table, run against client functions that were deliberately broken to return a fallback value WITH their error, reports every one.

// isNilValue reports whether v holds nothing: a nil interface, or a nil pointer, map, slice, channel or function.
func isNilValue(v any) bool {
	if v == nil {
		return true
	}
	switch rv := reflect.ValueOf(v); rv.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Chan, reflect.Func, reflect.Interface:
		return rv.IsNil()
	}
	return false
}

// valueViolation says what is wrong with a returned pair: "" when it is fine (a value and no error, or an error and no value).
func valueViolation(val any, err error) string {
	switch {
	case err != nil && !isNilValue(val):
		return fmt.Sprintf("the value %T came back together with the error", val)
	case err == nil && isNilValue(val):
		return "neither a value nor an error came back"
	}
	return ""
}

type valueOp struct {
	name string
	// module is the module the operation's client is built for; the zero value means module 2 (every operation but the two of module 1 below).
	module Module
	call   func(ctx context.Context, c *Client) (any, error)
}

// targetShape and target stand for what an adapter decodes a response into.
var targetShape = Object(Req("missing_key", Str))

type target struct {
	MissingKey string `json:"missing_key"`
	Other      string `json:"other"`
}

func realOps() []valueOp {
	return []valueOp{
		{"Do", "", func(ctx context.Context, c *Client) (any, error) {
			r, err := c.Do(ctx, getReq("m2.report", "/v1/report"))
			return r, err
		}},
		{"Do then Decode", "", func(ctx context.Context, c *Client) (any, error) {
			r, err := c.Do(ctx, getReq("m2.report", "/v1/report"))
			if err != nil {
				return nil, err
			}
			var v target
			if err := Decode(r, &v, targetShape); err != nil {
				if v != (target{}) {
					return &v, err // the target was written on an error
				}
				return nil, err
			}
			return &v, nil
		}},
		{"Health", "", func(ctx context.Context, c *Client) (any, error) { h, err := c.Health(ctx); return h, err }},
		{"Ready", "", func(ctx context.Context, c *Client) (any, error) { r, err := c.Ready(ctx); return r, err }},
		{"Describe", "", func(ctx context.Context, c *Client) (any, error) { d, err := c.Describe(ctx); return d, err }},
		{"Vocabularies", "", func(ctx context.Context, c *Client) (any, error) { v, err := c.Vocabularies(ctx); return v, err }},
		// Phase 3 close-out: module 1 has a describe now, with a shape of its own (describeShapeM1), so its two operations are in the table too, on a client built for module 1.
		{"Describe (module 1)", M1, func(ctx context.Context, c *Client) (any, error) { d, err := c.Describe(ctx); return d, err }},
		{"Vocabularies (module 1)", M1, func(ctx context.Context, c *Client) (any, error) { v, err := c.Vocabularies(ctx); return v, err }},
	}
}

// mutantOps are the real operations with one deliberate defect each: on an error they return a plausible fallback value anyway ("probably healthy", "known vocabulary", "the
// half-read response", a decoded target). They are what a careless refactor would produce, and the table must catch every one.
func mutantOps() []valueOp {
	real := realOps()
	byName := map[string]valueOp{}
	for _, o := range real {
		byName[o.name] = o
	}
	wrap := func(name string, fallback func() any) valueOp {
		inner := byName[name]
		return valueOp{"mutant " + name, inner.module, func(ctx context.Context, c *Client) (any, error) {
			v, err := inner.call(ctx, c)
			if err != nil {
				return fallback(), err
			}
			return v, nil
		}}
	}
	return []valueOp{
		wrap("Do", func() any { return &Response{Status: 200, Body: []byte(`{}`)} }),
		wrap("Do then Decode", func() any { return &target{MissingKey: "default"} }),
		wrap("Health", func() any { return &Health{Status: "ok", At: time.Now()} }),
		wrap("Ready", func() any { return &Readiness{Ready: true, Status: "ready"} }),
		wrap("Describe", func() any { return &Describe{Module: "send_managed", APIVersion: "v1"} }),
		wrap("Vocabularies", func() any { return Vocabularies{"job_state": {"queued", "sent"}} }),
		// the fallbacks module 1's two operations must never produce: "the module's own vocabulary, built in" (the very thing module 1 had before it had a describe)
		wrap("Describe (module 1)", func() any { return &Describe{Module: "verify", APIVersion: "v1", Raw: []byte(`{}`)} }),
		wrap("Vocabularies (module 1)", func() any { return Vocabularies{"status": {"valid", "invalid", "risky", "unknown"}} }),
	}
}

// failureCell is one way a call can fail, as a server behaviour (or the lack of a server) and the kind every operation must report for it.
type failureCell struct {
	name      string
	kind      Kind
	handler   func(w http.ResponseWriter, r *http.Request, n int)
	refused   bool            // close the server before the call
	cancelled bool            // call with a context that is already cancelled
	notAnErr  map[string]bool // operations for which this answer is a success (a valid JSON document is not an error for plain Do)
}

func replyWith(code int, ctype, body string) func(w http.ResponseWriter, r *http.Request, n int) {
	return func(w http.ResponseWriter, r *http.Request, n int) {
		if ctype != "" {
			w.Header().Set("Content-Type", ctype)
		}
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}
}

func failureCells() []failureCell {
	justDo := map[string]bool{"Do": true}
	return []failureCell{
		{name: "connection refused", kind: Unavailable, refused: true},
		{name: "the context is already cancelled", kind: Canceled, cancelled: true},
		{name: "400", kind: Refused, handler: replyWith(400, "application/json", m2Validation)},
		{name: "401", kind: Refused, handler: replyWith(401, "application/json", m2Unauth)},
		{name: "403", kind: Refused, handler: replyWith(403, "application/json", adminForbidden)},
		{name: "404", kind: NotFound, handler: replyWith(404, "application/json", m2NotFound)},
		{name: "409", kind: Refused, handler: replyWith(409, "application/json", m2Conflict)},
		{name: "422", kind: Refused, handler: replyWith(422, "application/json", m3Unprocess)},
		{name: "429", kind: RateLimited, handler: replyWith(429, "application/json", m3RateLimited)},
		{name: "500", kind: Internal, handler: replyWith(500, "application/json", m2Internal)},
		{name: "502", kind: Unavailable, handler: replyWith(502, "text/html", "<html>bad gateway</html>")},
		{name: "503 unavailable", kind: Unavailable, handler: replyWith(503, "application/json", m1NotWired)},
		{name: "503 paused", kind: Paused, handler: replyWith(503, "application/json", m2Paused)},
		{name: "504", kind: Unavailable, handler: replyWith(504, "", "")},
		{name: "408", kind: Timeout, handler: replyWith(408, "", "")},
		{name: "no answer within the timeout", kind: Timeout, handler: func(w http.ResponseWriter, r *http.Request, n int) { <-r.Context().Done() }},
		{name: "a body that stalls", kind: Timeout, handler: func(w http.ResponseWriter, r *http.Request, n int) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			_, _ = io.WriteString(w, `{"status":"ok","at":`)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}},
		{name: "a reset connection", kind: Unavailable, handler: func(w http.ResponseWriter, r *http.Request, n int) {
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				if tcp, ok := conn.(*net.TCPConn); ok {
					_ = tcp.SetLinger(0)
				}
				_ = conn.Close()
			}
		}},
		{name: "a body cut short of its length", kind: Unavailable, handler: func(w http.ResponseWriter, r *http.Request, n int) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", "200")
			w.WriteHeader(200)
			_, _ = io.WriteString(w, `{"status":"ok"`)
		}},
		{name: "not JSON", kind: Contract, handler: replyWith(200, "application/json", "this is not json")},
		{name: "the wrong content type", kind: Contract, handler: replyWith(200, "text/html", `{"status":"ok","at":"2026-11-17T10:00:00Z"}`)},
		{name: "a truncated document", kind: Contract, handler: replyWith(200, "application/json", `{"status":"ok","at":"2026-11`)},
		{name: "data after the document", kind: Contract, handler: replyWith(200, "application/json", `{"status":"ok"} {"x":1}`)},
		{name: "a body over the limit", kind: Contract, handler: replyWith(200, "application/json", `{"pad":"`+strings.Repeat("x", 4000)+`"}`)},
		{name: "an unexpected 201", kind: Contract, handler: replyWith(201, "application/json", `{"status":"ok"}`)},
		{name: "an unexpected 204", kind: Contract, handler: replyWith(204, "", "")},
		{name: "a redirect", kind: Contract, handler: func(w http.ResponseWriter, r *http.Request, n int) {
			http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
		}},
		{name: "a malformed error envelope", kind: Contract, handler: replyWith(400, "application/json", `{"error":"bad input"}`)},
		{name: "a valid document without the keys the adapter needs", kind: Contract, handler: replyWith(200, "application/json", `{"unrelated":1}`), notAnErr: justDo},
		{name: "a valid document of the wrong type", kind: Contract, handler: replyWith(200, "application/json", `[1,2,3]`), notAnErr: justDo},
		{name: "an empty body", kind: Contract, handler: replyWith(200, "application/json", ``), notAnErr: justDo},
		{name: "JSON null", kind: Contract, handler: replyWith(200, "application/json", `null`), notAnErr: justDo},
		// Phase 3 close-out D4: the liveness branch. A 200 that is a well-formed liveness document whose status is not "ok" is not a healthy module: Health must fail (Contract) on it, and every
		// other operation fails on it because the document lacks the keys they need. Found by the rehearsal of the modclient-no-value fault proof: a mutation of that branch was not caught.
		{name: "a 200 whose liveness status is not ok", kind: Contract, handler: replyWith(200, "application/json", `{"status":"degraded","at":"2026-11-17T10:00:00Z"}`), notAnErr: justDo},
	}
}

// runNoValueTable runs every operation against every failure and returns what it found wrong: a call that did not fail as the cell says, or one that returned a value with its
// error. cells is how many (operation, failure) pairs it ran, so a table that ran nothing cannot read as clean.
func runNoValueTable(t *testing.T, ops []valueOp) (violations []string, cells int) {
	t.Helper()
	for _, cell := range failureCells() {
		for _, op := range ops {
			baseName := strings.TrimPrefix(op.name, "mutant ")
			if cell.notAnErr[baseName] {
				continue
			}
			var s *stub
			if cell.handler != nil {
				s = newStub(t, cell.handler)
			} else {
				s = newStub(t, func(w http.ResponseWriter, r *http.Request, n int) {})
			}
			module := op.module
			if module == "" {
				module = M2
			}
			c := s.client(t, module, mustToken(t), Options{Timeout: 150 * time.Millisecond, MaxBodyBytes: 2048})
			if cell.refused {
				s.srv.Close()
				c.CloseIdleConnections()
			}
			ctx := ctxT(t, 20*time.Second)
			if cell.cancelled {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			val, err := op.call(ctx, c)
			cells++
			label := op.name + " / " + cell.name
			if err == nil {
				violations = append(violations, label+": the cell was meant to fail and returned no error")
				continue
			}
			if got := KindOf(err); got != cell.kind {
				violations = append(violations, fmt.Sprintf("%s: kind %s, want %s", label, got, cell.kind))
			}
			if why := valueViolation(val, err); why != "" {
				violations = append(violations, label+": "+why)
			}
		}
	}
	return violations, cells
}

func TestNoErrorPathReturnsAValue(t *testing.T) {
	ops := realOps()
	violations, cells := runNoValueTable(t, ops)
	// the table is not vacuous: every operation against every failure it applies to
	want := 0
	for _, cell := range failureCells() {
		for _, op := range ops {
			if !cell.notAnErr[op.name] {
				want++
			}
		}
	}
	if cells != want || cells < 150 {
		t.Fatalf("the table ran %d cells, want %d (and at least 150)", cells, want)
	}
	for _, v := range violations {
		t.Error(v)
	}

	// the same on the paths that do not need a server
	c, err := New(M1, "http://127.0.0.1:1", "tok-x", transport(t), Options{})
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctxT(t, time.Second))
	cancel()
	for name, call := range map[string]func() (any, error){
		// module 1 has a describe now: with a context that is already over, the call is not made and the answer is Canceled with no value (never NotSupported, never a built-in set)
		"Describe of module 1, context over":     func() (any, error) { v, err := c.Describe(cancelled); return v, err },
		"Vocabularies of module 1, context over": func() (any, error) { v, err := c.Vocabularies(cancelled); return v, err },
		"Do of an invalid request": func() (any, error) {
			v, err := c.Do(ctxT(t, time.Second), Request{Route: "BAD", Method: "GET", Path: "/"})
			return v, err
		},
		"Decode of a nil response": func() (any, error) { var v target; err := Decode(nil, &v, targetShape); return orNil(v), err },
		"New of a bad argument":    func() (any, error) { v, err := New(M1, "ftp://x", "tok", nil, Options{}); return v, err },
	} {
		val, err := call()
		if why := valueViolation(val, err); why != "" {
			t.Errorf("%s: %s", name, why)
		}
	}
}

// orNil turns a zero target into nil, so that "the target was left alone" reads as "no value".
func orNil(v target) any {
	if v == (target{}) {
		return nil
	}
	return &v
}

func TestNoValueCheck_CatchesAClientThatInventsAFallback(t *testing.T) {
	// The checker itself, on hand-made pairs: it accepts (nil, error) and (value, nil) and refuses (value, error) for every kind of value a method here returns.
	boom := fmt.Errorf("boom")
	for name, c := range map[string]struct {
		val  any
		err  error
		want bool // a violation is expected
	}{
		"nil and an error":                 {nil, boom, false},
		"a nil pointer and an error":       {(*Health)(nil), boom, false},
		"a nil map and an error":           {Vocabularies(nil), boom, false},
		"a value and no error":             {&Health{Status: "ok"}, nil, false},
		"a pointer and an error":           {&Health{Status: "ok"}, boom, true},
		"an empty map and an error":        {Vocabularies{}, boom, true},
		"a response and an error":          {&Response{}, boom, true},
		"a value and an error":             {Readiness{}, boom, true},
		"no value and no error (nonsense)": {nil, nil, true},
	} {
		if got := valueViolation(c.val, c.err) != ""; got != c.want {
			t.Errorf("%s: violation = %v, want %v", name, got, c.want)
		}
	}

	// The mutants: each is a real operation that falls back to a plausible value when the call fails. The table must flag every one of them in every cell.
	mutants := mutantOps()
	violations, cells := runNoValueTable(t, mutants)
	invented := map[string]int{}
	for _, v := range violations {
		if strings.Contains(v, "came back together with the error") {
			invented[strings.SplitN(v, " / ", 2)[0]]++
		}
	}
	for _, m := range mutants {
		if invented[m.name] == 0 {
			t.Errorf("the table did not catch %s", m.name)
		}
	}
	if cells < 150 || len(invented) != len(mutants) {
		t.Fatalf("cells %d, mutants caught %d of %d: %v", cells, len(invented), len(mutants), invented)
	}
	// and it catches them in every cell, not in a lucky one: as many violations as cells that failed with an error
	total := 0
	for _, n := range invented {
		total += n
	}
	if total != cells {
		t.Fatalf("the mutants were flagged in %d of %d cells", total, cells)
	}
}
