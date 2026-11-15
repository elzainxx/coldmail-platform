package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/httpapi"
	"github.com/coldmail-platform/platform/internal/logging"
)

// These tests run the REAL server on a REAL listener (127.0.0.1, port chosen by the OS). There is no
// httptest.Server and no mocked transport: the properties under test (a connection is refused, an
// in-flight request survives a shutdown) are properties of sockets.

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) lines(t *testing.T) []map[string]any {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	for _, raw := range strings.Split(strings.TrimSpace(s.b.String()), "\n") {
		if raw == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", raw, err)
		}
		out = append(out, m)
	}
	return out
}

type okPinger struct{}

func (okPinger) Ping(context.Context) error { return nil }

// gate is a channel that can be opened any number of times from any goroutine.
type gate struct {
	ch   chan struct{}
	once sync.Once
}

func newGate() *gate               { return &gate{ch: make(chan struct{})} }
func (g *gate) open()              { g.once.Do(func() { close(g.ch) }) }
func (g *gate) c() <-chan struct{} { return g.ch }

// fixture is the handler under the real listener: the platform's own /healthz and /readyz, plus two
// test-only routes that stay in flight until released.
//
//	/slow   blocks until releaseSlow opens, then answers 200 "done"
//	/probe  blocks until releaseProbe opens, then runs the REAL readyz handler: a readiness probe that
//	        was already in flight when the drain began
type fixture struct {
	handler      http.Handler
	draining     atomic.Bool
	slowStarted  *gate
	probeStarted *gate
	releaseSlow  *gate
	releaseProbe *gate
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{slowStarted: newGate(), probeStarted: newGate(), releaseSlow: newGate(), releaseProbe: newGate()}
	inner := httpapi.NewHandler(httpapi.Deps{
		Pinger:       okPinger{},
		Draining:     f.draining.Load,
		ReadyTimeout: time.Second,
	})
	mux := http.NewServeMux()
	mux.HandleFunc("/slow", func(w http.ResponseWriter, _ *http.Request) {
		f.slowStarted.open()
		<-f.releaseSlow.c()
		_, _ = w.Write([]byte("done"))
	})
	mux.HandleFunc("/probe", func(w http.ResponseWriter, r *http.Request) {
		f.probeStarted.open()
		<-f.releaseProbe.c()
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/readyz"
		inner.ServeHTTP(w, r2)
	})
	mux.Handle("/", inner)
	f.handler = mux
	// Whatever a test leaves blocked is released so no handler goroutine outlives it.
	t.Cleanup(func() { f.releaseSlow.open(); f.releaseProbe.open() })
	return f
}

type runner func(ctx context.Context, cfg ServerConfig, h http.Handler, setDraining func(bool), log *slog.Logger) error

// start launches run on 127.0.0.1:0 and returns once it is listening.
func start(t *testing.T, f *fixture, run runner, cfg ServerConfig) (addr string, cancel context.CancelFunc, done <-chan error, logs *syncBuffer) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	logs = &syncBuffer{}
	log, err := logging.New("debug", logs)
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan net.Addr, 1)
	cfg.Addr, cfg.Ready = "127.0.0.1:0", ready
	errc := make(chan error, 1)
	go func() { errc <- run(ctx, cfg, f.handler, f.draining.Store, log) }()
	select {
	case a := <-ready:
		return a.String(), cancel, errc, logs
	case err := <-errc:
		t.Fatalf("the runner returned before it was listening: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("the runner never reported its address")
	}
	return "", nil, nil, nil
}

func client() *http.Client {
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
}

type result struct {
	status int
	body   string
	err    error
}

func get(url string) <-chan result {
	out := make(chan result, 1)
	go func() {
		resp, err := client().Get(url)
		if err != nil {
			out <- result{err: err}
			return
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		out <- result{status: resp.StatusCode, body: string(b), err: err}
	}()
	return out
}

func await(t *testing.T, c <-chan result, what string) result {
	t.Helper()
	select {
	case r := <-c:
		return r
	case <-time.After(8 * time.Second):
		t.Fatalf("%s never completed", what)
		return result{}
	}
}

func waitGate(g *gate, d time.Duration) bool {
	select {
	case <-g.c():
		return true
	case <-time.After(d):
		return false
	}
}

// ---- the property, as an instrument -----------------------------------------------------------------------

// observation is everything checkGracefulDrain looks at.
type observation struct {
	setup string // non-empty when the scenario could not be staged; reported as a violation

	preReadyz int   // GET /readyz before the cancel
	preDial   error // dial before the cancel

	drainingSet   bool
	returnedEarly bool // Run returned while both requests were still in flight
	probe, slow   result
	returned      bool
	doneErr       error
	dialAfter     error // nil means the address still accepted a connection
}

const (
	vSetup     = "scenario could not be staged: "
	vPre       = "precondition: the server was not serving normally before the cancel (readyz not 200, or not reachable), so the checks below prove nothing"
	vDrainFlag = "draining was not set after the context was cancelled (readyz cannot flip to 503)"
	vReadyz    = "readyz did not answer 503 to a probe that was in flight during the drain"
	vInflight  = "an in-flight request did not complete with 200 \"done\" after the context was cancelled"
	vEarly     = "Run returned while requests were still in flight (connections were cut instead of drained)"
	vNoReturn  = "Run did not return after the in-flight requests finished"
	vCleanErr  = "Run returned an error after a clean drain"
	vRefuse    = "the address still accepts connections after Run returned"
)

// observeDrain stages one scenario against a running server and records what happened: one request and
// one readiness probe are put in flight, the context is cancelled, the probe is released while the slow
// request is still blocked, then the slow request is released.
func observeDrain(f *fixture, addr string, cancel context.CancelFunc, done <-chan error) observation {
	var o observation
	base := "http://" + addr

	// Preconditions: serving, readyz 200, reachable.
	pre := <-get(base + "/readyz")
	o.preReadyz = pre.status
	if c, err := net.DialTimeout("tcp", addr, 2*time.Second); err != nil {
		o.preDial = err
	} else {
		_ = c.Close()
	}

	slow, probe := get(base+"/slow"), get(base+"/probe")
	if !waitGate(f.slowStarted, 5*time.Second) || !waitGate(f.probeStarted, 5*time.Second) {
		o.setup = "the in-flight requests never reached their handlers"
		cancel()
		return o
	}

	cancel()

	// Draining must be visible promptly.
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if f.draining.Load() {
			o.drainingSet = true
			break
		}
	}

	// With two requests still blocked, Run must NOT have returned.
	select {
	case err := <-done:
		o.returnedEarly, o.returned, o.doneErr = true, true, err
	case <-time.After(150 * time.Millisecond):
	}

	f.releaseProbe.open()
	o.probe = collect(probe)
	f.releaseSlow.open()
	o.slow = collect(slow)

	if !o.returned {
		select {
		case err := <-done:
			o.returned, o.doneErr = true, err
		case <-time.After(5 * time.Second):
		}
	}
	if c, err := net.DialTimeout("tcp", addr, 2*time.Second); err == nil {
		_ = c.Close()
		o.dialAfter = errAccepted
	} else {
		o.dialAfter = err
	}
	return o
}

var errAccepted = errors.New("accepted") // sentinel: the dial succeeded

func collect(c <-chan result) result {
	select {
	case r := <-c:
		return r
	case <-time.After(8 * time.Second):
		return result{err: errors.New("no response within 8s")}
	}
}

// violations turns an observation into the list of broken properties (empty when the drain was graceful).
func violations(o observation) []string {
	var v []string
	if o.setup != "" {
		return []string{vSetup + o.setup}
	}
	if o.preReadyz != http.StatusOK || o.preDial != nil {
		v = append(v, vPre)
	}
	if !o.drainingSet {
		v = append(v, vDrainFlag)
	}
	if o.probe.err != nil || o.probe.status != http.StatusServiceUnavailable || !strings.Contains(o.probe.body, `"not_ready"`) {
		v = append(v, vReadyz)
	}
	if o.slow.err != nil || o.slow.status != http.StatusOK || o.slow.body != "done" {
		v = append(v, vInflight)
	}
	if o.returnedEarly {
		v = append(v, vEarly)
	}
	if !o.returned {
		v = append(v, vNoReturn)
	} else if o.doneErr != nil {
		v = append(v, vCleanErr)
	}
	if o.dialAfter == errAccepted {
		v = append(v, vRefuse)
	}
	return v
}

// checkGracefulDrain stages the scenario against whatever is listening at addr and returns the list of
// violated properties; an empty list means the drain was graceful. It is run against the real Run and,
// as the control, against deliberately broken runners.
func checkGracefulDrain(f *fixture, addr string, cancel context.CancelFunc, done <-chan error) []string {
	return violations(observeDrain(f, addr, cancel, done))
}

func realRun(t *testing.T) (*fixture, string, context.CancelFunc, <-chan error) {
	t.Helper()
	f := newFixture(t)
	addr, cancel, done, _ := start(t, f, Run, ServerConfig{ReadHeaderTimeout: 5 * time.Second, ShutdownTimeout: 10 * time.Second})
	return f, addr, cancel, done
}

// ---- Run, one property per test ---------------------------------------------------------------------------

func TestRun_AnInFlightRequestCompletesAfterTheContextIsCancelled(t *testing.T) { // (a)
	f, addr, cancel, done := realRun(t)
	o := observeDrain(f, addr, cancel, done)

	if o.setup != "" {
		t.Fatal(o.setup)
	}
	if o.slow.err != nil || o.slow.status != http.StatusOK || o.slow.body != "done" {
		t.Fatalf("the in-flight request got %+v, want 200 \"done\"", o.slow)
	}
	if o.returnedEarly {
		t.Fatal("Run had already returned while the request was in flight: it cut the connection rather than draining")
	}
	// Paired control: the same request, before any cancel, is simply served (the route works at all).
	f2 := newFixture(t)
	addr2, cancel2, done2, _ := start(t, f2, Run, ServerConfig{ReadHeaderTimeout: time.Second, ShutdownTimeout: time.Second})
	f2.releaseSlow.open()
	if r := await(t, get("http://"+addr2+"/slow"), "uncancelled /slow"); r.status != 200 || r.body != "done" {
		t.Fatalf("control failed: /slow before any cancel = %+v", r)
	}
	cancel2()
	<-done2
}

func TestRun_ReadyzAnswers503DuringTheDrain(t *testing.T) { // (b)
	f, addr, cancel, done := realRun(t)
	o := observeDrain(f, addr, cancel, done)

	if o.setup != "" {
		t.Fatal(o.setup)
	}
	if o.preReadyz != http.StatusOK {
		t.Fatalf("control failed: readyz before the drain = %d, want 200 (otherwise a 503 later means nothing)", o.preReadyz)
	}
	if !o.drainingSet {
		t.Fatal("setDraining(true) was not called after the cancel")
	}
	if o.probe.err != nil || o.probe.status != http.StatusServiceUnavailable || !strings.Contains(o.probe.body, `"not_ready"`) {
		t.Fatalf("a probe in flight during the drain got %+v, want 503 not_ready", o.probe)
	}
	if o.returnedEarly {
		t.Fatal("control failed: Run had already returned when the probe was answered, so this was not 'during the drain'")
	}
}

func TestRun_RefusesConnectionsOnceItHasReturned(t *testing.T) { // (c)
	f, addr, cancel, done := realRun(t)
	o := observeDrain(f, addr, cancel, done)

	if o.setup != "" {
		t.Fatal(o.setup)
	}
	// Paired control: before the cancel the same address ACCEPTED a connection.
	if o.preDial != nil {
		t.Fatalf("control failed: the address was not reachable before the cancel: %v", o.preDial)
	}
	if !o.returned || o.doneErr != nil {
		t.Fatalf("Run returned=%v err=%v, want a clean return", o.returned, o.doneErr)
	}
	if o.dialAfter == errAccepted {
		t.Fatal("the address still accepts connections after Run returned")
	}
	var ne net.Error
	if !errors.As(o.dialAfter, &ne) {
		t.Fatalf("the dial failed with %v, want a network error (connection refused)", o.dialAfter)
	}
}

func TestRun_ADrainThatOutlastsTheTimeoutReturnsAnError(t *testing.T) { // (d)
	const timeout = 200 * time.Millisecond
	f := newFixture(t)
	addr, cancel, done, logs := start(t, f, Run, ServerConfig{ReadHeaderTimeout: time.Second, ShutdownTimeout: timeout})

	slow := get("http://" + addr + "/slow")
	if !waitGate(f.slowStarted, 5*time.Second) {
		t.Fatal("the slow request never reached its handler")
	}
	t0 := time.Now()
	cancel() // the slow request is NEVER released inside the timeout

	var err error
	select {
	case err = <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("Run did not return after the shutdown timeout")
	}
	took := time.Since(t0)

	if err == nil {
		t.Fatal("Run returned nil although a request outlived the shutdown timeout")
	}
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "did not finish within") {
		t.Errorf("error = %v, want one wrapping context.DeadlineExceeded that names the timeout", err)
	}
	if took < timeout-20*time.Millisecond {
		t.Errorf("Run returned after %v, before the %v timeout: it did not wait", took, timeout)
	}
	if took > 5*time.Second {
		t.Errorf("Run took %v, far past the %v timeout", took, timeout)
	}
	if !f.draining.Load() {
		t.Error("draining was not set")
	}
	// The straggler was cut, not left hanging, and the address is closed.
	if r := await(t, slow, "the straggler"); r.err == nil {
		t.Errorf("the request that outlived the timeout still got a response: %+v", r)
	}
	if c, derr := net.DialTimeout("tcp", addr, 2*time.Second); derr == nil {
		_ = c.Close()
		t.Error("the address still accepts connections after a timed-out drain")
	}
	var logged bool
	for _, l := range logs.lines(t) {
		logged = logged || strings.Contains(l["msg"].(string), "drain timed out")
	}
	if !logged {
		t.Error("the timed-out drain was not logged")
	}
}

// ---- the paired control -----------------------------------------------------------------------------------

func TestRun_TheGracefulDrainPropertyHasNoViolations(t *testing.T) {
	f, addr, cancel, done := realRun(t)
	if v := checkGracefulDrain(f, addr, cancel, done); len(v) != 0 {
		t.Fatalf("the real Run violates the graceful-drain properties:\n  %s", strings.Join(v, "\n  "))
	}
}

// brokenRun is a deliberately wrong runner: it serves like Run but, on cancel, optionally skips the
// draining flag and optionally closes the server instead of shutting it down.
func brokenRun(flipDraining, graceful bool) runner {
	return func(ctx context.Context, cfg ServerConfig, h http.Handler, setDraining func(bool), _ *slog.Logger) error {
		ln, err := net.Listen("tcp", cfg.Addr)
		if err != nil {
			return err
		}
		srv := &http.Server{Handler: h, ReadHeaderTimeout: cfg.ReadHeaderTimeout}
		go func() { _ = srv.Serve(ln) }()
		cfg.Ready <- ln.Addr()
		<-ctx.Done()
		if flipDraining {
			setDraining(true)
		}
		if graceful {
			sctx, c := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
			defer c()
			return srv.Shutdown(sctx)
		}
		return srv.Close()
	}
}

func TestCheckGracefulDrain_NamesTheBrokenPropertyOfEachBrokenRunner(t *testing.T) {
	mutants := []struct {
		name         string
		run          runner
		want, absent []string
	}{
		{
			name:   "srv.Close() instead of Shutdown, and no draining flag (the task's example)",
			run:    brokenRun(false, false),
			want:   []string{vDrainFlag, vReadyz, vInflight, vEarly},
			absent: []string{vPre, vCleanErr, vNoReturn, vRefuse},
		},
		{
			name:   "srv.Close() instead of Shutdown, but the draining flag is set",
			run:    brokenRun(true, false),
			want:   []string{vReadyz, vInflight, vEarly},
			absent: []string{vPre, vDrainFlag, vCleanErr, vNoReturn, vRefuse},
		},
		{
			name:   "graceful Shutdown, but the draining flag is never set",
			run:    brokenRun(false, true),
			want:   []string{vDrainFlag, vReadyz},
			absent: []string{vPre, vInflight, vEarly, vCleanErr, vNoReturn, vRefuse},
		},
	}
	for _, m := range mutants {
		t.Run(m.name, func(t *testing.T) {
			f := newFixture(t)
			addr, cancel, done, _ := start(t, f, m.run, ServerConfig{ReadHeaderTimeout: 5 * time.Second, ShutdownTimeout: 10 * time.Second})
			got := checkGracefulDrain(f, addr, cancel, done)
			if len(got) == 0 {
				t.Fatal("the check found NO violation in a broken runner: it cannot fail, so it proves nothing about the real Run")
			}
			sort.Strings(got)
			want := append([]string(nil), m.want...)
			sort.Strings(want)
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Fatalf("violations reported:\n  %s\nwant exactly:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
			}
			for _, a := range m.absent {
				for _, g := range got {
					if g == a {
						t.Errorf("reported %q, which this runner does not break", a)
					}
				}
			}
		})
	}
}

// ---- configuration reaches the server ---------------------------------------------------------------------

// readUntilClosed sends a partial request (headers never completed) and reports how the server reacted.
func readUntilClosed(t *testing.T, addr string, wait time.Duration) (closedByServer bool, took time.Duration) {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("GET /healthz HTTP/1.1\r\nHost: test\r\n")); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	_ = c.SetReadDeadline(t0.Add(wait))
	buf := make([]byte, 64)
	_, err = c.Read(buf)
	took = time.Since(t0)
	if err == nil {
		t.Fatal("the server answered an incomplete request")
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return false, took // our own deadline fired: the server never gave up
	}
	return true, took
}

func TestRun_ReadHeaderTimeoutComesFromTheConfig(t *testing.T) {
	// Short timeout: the server closes the half-sent request on its own.
	f := newFixture(t)
	addr, cancel, done, _ := start(t, f, Run, ServerConfig{ReadHeaderTimeout: 200 * time.Millisecond, ShutdownTimeout: time.Second})
	closed, took := readUntilClosed(t, addr, 5*time.Second)
	if !closed || took < 150*time.Millisecond || took > 4*time.Second {
		t.Errorf("short ReadHeaderTimeout: closedByServer=%v after %v, want a close at about 200ms", closed, took)
	}
	cancel()
	<-done

	// Paired control: with a long timeout the SAME half-sent request is still open when our own deadline
	// (much longer than the short timeout above) expires, so the close above was the timeout's doing.
	f2 := newFixture(t)
	addr2, cancel2, done2, _ := start(t, f2, Run, ServerConfig{ReadHeaderTimeout: 30 * time.Second, ShutdownTimeout: time.Second})
	closed, took = readUntilClosed(t, addr2, 800*time.Millisecond)
	if closed || took < 700*time.Millisecond {
		t.Errorf("long ReadHeaderTimeout: closedByServer=%v after %v, want the connection still open at 800ms", closed, took)
	}
	cancel2()
	<-done2
}

func TestRun_ReadyReportsTheBoundAddressAndListenErrorsAreReturned(t *testing.T) {
	f := newFixture(t)
	addr, cancel, done, _ := start(t, f, Run, ServerConfig{ReadHeaderTimeout: time.Second, ShutdownTimeout: time.Second})
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host != "127.0.0.1" || port == "0" || port == "" {
		t.Fatalf("Ready reported %q (%v), want 127.0.0.1 and the port the OS chose", addr, err)
	}
	if r := await(t, get("http://"+addr+"/healthz"), "healthz"); r.status != 200 || !strings.Contains(r.body, `"ok"`) {
		t.Fatalf("the reported address does not serve /healthz: %+v", r)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("clean drain returned %v", err)
	}

	// An address already in use is an error from Run, immediately, and it never calls setDraining.
	busy, lerr := net.Listen("tcp", "127.0.0.1:0")
	if lerr != nil {
		t.Fatal(lerr)
	}
	defer busy.Close()
	var flipped atomic.Bool
	errc := make(chan error, 1)
	go func() {
		errc <- Run(context.Background(), ServerConfig{Addr: busy.Addr().String(), ShutdownTimeout: time.Second}, f.handler, func(bool) { flipped.Store(true) }, nil)
	}()
	select {
	case err := <-errc:
		if err == nil || !strings.Contains(err.Error(), "listen") {
			t.Fatalf("Run on a busy address returned %v, want a listen error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run on a busy address did not return")
	}
	if flipped.Load() {
		t.Error("a server that never started claimed to be draining")
	}
}

func TestRun_ACancelledContextNeverWedgesRun(t *testing.T) {
	f := newFixture(t)

	// Cancelled before Run is even called, and nobody reads Ready: Run still returns, cleanly.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	unread := make(chan net.Addr) // unbuffered and never read
	errc := make(chan error, 1)
	go func() {
		errc <- Run(ctx, ServerConfig{Addr: "127.0.0.1:0", ShutdownTimeout: time.Second, Ready: unread}, f.handler, f.draining.Store, nil)
	}()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("Run with a pre-cancelled context returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run wedged on an unread Ready channel after cancellation")
	}
	if !f.draining.Load() {
		t.Error("a pre-cancelled Run did not mark the service draining")
	}

	// Paired control: with a live context and a buffered Ready the same call serves.
	f2 := newFixture(t)
	addr, cancel2, done2, _ := start(t, f2, Run, ServerConfig{ReadHeaderTimeout: time.Second, ShutdownTimeout: time.Second})
	if r := await(t, get("http://"+addr+"/healthz"), "healthz"); r.status != 200 {
		t.Fatalf("control failed: %+v", r)
	}
	cancel2()
	<-done2
}

func TestRun_LogsListeningDrainingAndStopped(t *testing.T) {
	f := newFixture(t)
	addr, cancel, done, logs := start(t, f, Run, ServerConfig{ReadHeaderTimeout: time.Second, ShutdownTimeout: 3 * time.Second})
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	order := map[string]int{}
	for i, l := range logs.lines(t) {
		msg, _ := l["msg"].(string)
		if _, seen := order[msg]; !seen {
			order[msg] = i
		}
		if msg == "http server listening" && l["addr"] != addr {
			t.Errorf("the listening line says %v, the bound address is %s", l["addr"], addr)
		}
	}
	l, d, s := order["http server listening"], order["http server draining"], order["http server stopped"]
	if len(order) < 3 || !(l < d && d < s) {
		t.Fatalf("expected listening < draining < stopped, got %v", order)
	}
	if _, bad := order["http server drain timed out; remaining connections were closed"]; bad {
		t.Error("a clean drain logged a timeout")
	}
}
