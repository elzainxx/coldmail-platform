//go:build harness

package harness

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	m1ad "github.com/coldmail-platform/platform/internal/adapters/m1"
	"github.com/coldmail-platform/platform/internal/adapters/m2"
	"github.com/coldmail-platform/platform/internal/adapters/m3"
	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/jobengine"
	"github.com/coldmail-platform/platform/internal/jobengine/jobenginetest"
	"github.com/coldmail-platform/platform/internal/jobs"
	"github.com/coldmail-platform/platform/internal/modclient"
	"github.com/coldmail-platform/platform/internal/money"
	"github.com/coldmail-platform/platform/internal/tenant"
)

// Phase 4, chunks B8 to B11, the part the container-stop proofs cannot do: the job engine of THIS process against the REAL modules while the platform's PATH to a module is cut and restored. The engine reaches a module
// through an in-process TCP forwarder (jlLink): a listener on the loopback that copies bytes both ways to the module's real host and port inside the harness network. Cut closes the listener (a new connection is
// REFUSED by the kernel) and closes every connection in flight (the adapters' keep-alive connections included); Open listens again on the same address. The module itself keeps running, sends and settles while the
// link is down: that is the point. The test's own readers (the module's report, its tables, the sink) never go through the forwarder: they are the independent witness of what the module did meanwhile.
//
// THE ENGINE IS THE PRODUCTION ONE over the production adapters, built as internal/app builds them (newJlStackWith is newJlStack with the module URLs, the staleness dial and the start delay as parameters); the
// registry is decorated by jlTrace, which records every Submit, Observe and Cancel the engine makes of the module and what each answered (ok, wait, refused, other): the "refusals are waits" claim is read from that
// record, not assumed. The forwarder assumes the module speaks PLAIN HTTP on the private network (it does: every URL of the harness stack is http://), so that the Host header the module sees names 127.0.0.1 and no
// certificate is involved; a module that checked its Host header would answer the engine's first request with an error and the first assertion of every test here (the engine reaches the module through the link)
// would name it.
//
// THE PAIRED CONTROLS. Every outage test is followed by the SAME job with the link never cut (and the default staleness dial): it succeeds the same way, gains results while the outage test's job gained none, and
// raises no `stale` event, so the outage is what the assertions about waiting measure. The link itself is proved by probes: reachable before the cut, refused after it (and at every tick of the outage), reachable
// again after Open; and the engine's own traffic must have crossed the forwarder before the cut (connections accepted, bytes both ways).

const (
	// jlOutageStale is the staleness dial of the outage engines (the service's default is 15 and 30 minutes): a running job that makes no progress for this long raises a `stale` event.
	jlOutageStale = 6 * time.Second
	// jlOutageHold is how long the link stays cut at least; jlOutageMaxHold is how long it stays cut at most while waiting for the module to get ahead of the platform.
	jlOutageHold    = 20 * time.Second
	jlOutageMaxHold = 100 * time.Second
	// jlExpiryDelay is the max start delay of the expiry tests' store: a job the module never takes expires this long after its confirm (the database's clock).
	jlExpiryDelay = 30 * time.Second // was 10 s: the control job of the expiry test must START inside its own deadline and did not once on a loaded runner (the first start waits for module 3 to take its first run call after the link reopened); a longer deadline lengthens both halves of the test and changes no property it proves
)

// ---- the link -----------------------------------------------------------------------------------------------------------------------------------------------------------------

// jlLink is the forwarder. It is safe for concurrent use. It leaves nothing behind: the cleanup closes the listener and every connection and waits (bounded) for every goroutine it started.
type jlLink struct {
	t      *testing.T
	name   string
	target string // host:port of the module inside the harness network: what the forwarder dials
	addr   string // 127.0.0.1:port: what the platform's adapters dial
	prefix string // the path prefix of the module's base URL ("" for none)

	mu       sync.Mutex
	ln       net.Listener
	conns    map[net.Conn]struct{}
	up       bool
	finished bool
	wg       sync.WaitGroup

	accepted atomic.Int64 // connections accepted
	toModule atomic.Int64 // bytes copied towards the module
	toClient atomic.Int64 // bytes copied towards the platform
}

// newJlLink starts a forwarder to the module at rawURL (open). It FAILS for a URL it cannot forward: https (the certificate would name the module and not 127.0.0.1) or no host.
func newJlLink(t *testing.T, name, rawURL string) *jlLink {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "http" || u.Hostname() == "" {
		t.Fatalf("the %s forwarder needs the module's base URL to be plain http with a host (the harness network speaks http between the platform and its modules; with TLS the certificate would name the module, not the loopback address)", name)
	}
	port := u.Port()
	if port == "" {
		port = "80"
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("the %s forwarder could not listen on the loopback: %v", name, err)
	}
	l := &jlLink{t: t, name: name, target: net.JoinHostPort(u.Hostname(), port), addr: ln.Addr().String(), prefix: strings.TrimRight(u.EscapedPath(), "/"),
		ln: ln, conns: map[net.Conn]struct{}{}, up: true}
	l.wg.Add(1)
	go l.acceptLoop(ln)
	t.Cleanup(l.shutdown)
	return l
}

// URL is the base URL the platform's adapters are given instead of the module's own.
func (l *jlLink) URL() string { return "http://" + l.addr + l.prefix }

func (l *jlLink) acceptLoop(ln net.Listener) {
	defer l.wg.Done()
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			l.mu.Lock()
			current := l.ln == ln
			l.mu.Unlock()
			if !current {
				return
			}
			time.Sleep(10 * time.Millisecond) // a transient accept error (descriptors): try again while this listener is the link's
			continue
		}
		l.accepted.Add(1)
		l.wg.Add(1)
		go l.serve(c)
	}
}

// serve copies one connection to the module and back until either side ends. A connection accepted while the link is being cut is closed at once.
func (l *jlLink) serve(c net.Conn) {
	defer l.wg.Done()
	up, err := net.DialTimeout("tcp", l.target, 5*time.Second)
	if err != nil {
		_ = c.Close()
		return
	}
	if !l.track(c, up) {
		_ = c.Close()
		_ = up.Close()
		return
	}
	var once sync.Once
	shut := func() {
		once.Do(func() {
			_ = c.Close()
			_ = up.Close()
			l.untrack(c, up)
		})
	}
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		_, _ = io.Copy(jlCountWriter{w: up, n: &l.toModule}, c)
		shut()
	}()
	_, _ = io.Copy(jlCountWriter{w: c, n: &l.toClient}, up)
	shut()
}

// jlCountWriter counts the bytes as they are copied, so that the forwarder's traffic is visible while a connection is still open (a keep-alive connection ends only when the link is cut).
type jlCountWriter struct {
	w io.Writer
	n *atomic.Int64
}

func (c jlCountWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n.Add(int64(n))
	return n, err
}

func (l *jlLink) track(cs ...net.Conn) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.up {
		return false
	}
	for _, c := range cs {
		l.conns[c] = struct{}{}
	}
	return true
}

func (l *jlLink) untrack(cs ...net.Conn) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, c := range cs {
		delete(l.conns, c)
	}
}

// Cut is the outage: the listener is closed (a new connection is refused) and every connection in flight is closed on both sides. It returns when both are done.
func (l *jlLink) Cut() {
	l.mu.Lock()
	if !l.up {
		l.mu.Unlock()
		return
	}
	l.up = false
	ln := l.ln
	l.ln = nil
	cs := make([]net.Conn, 0, len(l.conns))
	for c := range l.conns {
		cs = append(cs, c)
	}
	l.mu.Unlock()
	_ = ln.Close()
	for _, c := range cs {
		_ = c.Close()
	}
}

// Open ends the outage: the forwarder listens again on the very address the adapters were given (a bound retry: the address was this process's own a moment ago). It returns an error only when the address
// could not be taken again within ten seconds.
func (l *jlLink) Open() error {
	l.mu.Lock()
	if l.up || l.finished {
		l.mu.Unlock()
		return nil
	}
	l.mu.Unlock()
	var ln net.Listener
	var err error
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if ln, err = net.Listen("tcp", l.addr); err == nil || time.Now().After(deadline) {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("the %s forwarder could not listen again on its own address within 10 s (an outgoing connection of this process took the port?): %v", l.name, err)
	}
	l.mu.Lock()
	if l.up || l.finished {
		l.mu.Unlock()
		_ = ln.Close()
		return nil
	}
	l.ln, l.up = ln, true
	l.wg.Add(1)
	l.mu.Unlock()
	go l.acceptLoop(ln)
	return nil
}

// mustOpen is Open that fails the test.
func (l *jlLink) mustOpen() {
	l.t.Helper()
	if err := l.Open(); err != nil {
		l.t.Fatal(err)
	}
}

// shutdown is the cleanup: close everything and wait for every goroutine; a goroutine still alive after ten seconds is a defect of this file and fails the test.
func (l *jlLink) shutdown() {
	l.mu.Lock()
	l.finished, l.up = true, false
	ln := l.ln
	l.ln = nil
	cs := make([]net.Conn, 0, len(l.conns))
	for c := range l.conns {
		cs = append(cs, c)
	}
	l.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
	for _, c := range cs {
		_ = c.Close()
	}
	done := make(chan struct{})
	go func() { l.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		l.t.Errorf("the %s forwarder left goroutines behind: they did not end within 10 s of the link being closed", l.name)
	}
}

// reachable asks the module, THROUGH the link, for /healthz: any HTTP answer (a 404 included) is "the link carries a request"; a refused connection, a reset or a timeout is not.
func (l *jlLink) reachable() (bool, string) {
	tr := &http.Transport{DisableKeepAlives: true}
	defer tr.CloseIdleConnections()
	hc := &http.Client{Timeout: 5 * time.Second, Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := hc.Get("http://" + l.addr + l.prefix + "/healthz")
	if err != nil {
		var ne net.Error
		why := "no answer"
		switch {
		case errors.Is(err, syscall.ECONNREFUSED):
			why = "connection refused"
		case errors.As(err, &ne) && ne.Timeout():
			why = "timed out"
		}
		return false, why
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	return true, fmt.Sprintf("HTTP %d", resp.StatusCode)
}

func (l *jlLink) requireUp(when string) {
	l.t.Helper()
	if ok, why := l.reachable(); !ok {
		l.t.Fatalf("%s: the link to %s carries no request (%s)", when, l.name, why)
	}
}

func (l *jlLink) requireDown(when string) {
	l.t.Helper()
	if ok, why := l.reachable(); ok {
		l.t.Fatalf("%s: the link to %s still carries a request (%s): the cut did not bite, so nothing measured here was an outage", when, l.name, why)
	}
}

// requireCarried fails unless the engine's own traffic crossed the forwarder: connections were accepted and bytes went both ways. Without it a URL override that the adapters ignored would leave every
// "outage" assertion measuring nothing.
func (l *jlLink) requireCarried(when string) {
	l.t.Helper()
	if l.accepted.Load() == 0 || l.toModule.Load() == 0 || l.toClient.Load() == 0 {
		l.t.Fatalf("%s: the engine's traffic did not cross the %s forwarder (connections accepted %d, bytes to the module %d, bytes to the platform %d): the adapters were not talking through it", when, l.name, l.accepted.Load(), l.toModule.Load(), l.toClient.Load())
	}
}

// ---- what the engine asked of the module, and what it was told -------------------------------------------------------------------------------------------------------------

// jlCall is one call of the engine to a module: the operation, the job, and what came back.
type jlCall struct {
	Op   string // submit, observe or cancel
	Job  string
	Kind string // ok, wait (jobengine.ErrWait), refused (ErrRefused), unsupported (ErrNotSupported) or other
	Err  string // for a failure: the engine's own words (the wrappers write words only, never an address or a key)
}

// jlTrace records the calls of one engine. It is safe for concurrent use.
type jlTrace struct {
	mu    sync.Mutex
	calls []jlCall
}

func (tr *jlTrace) add(op, job string, err error) {
	c := jlCall{Op: op, Job: job, Kind: "ok"}
	switch {
	case err == nil:
	case errors.Is(err, jobengine.ErrWait):
		c.Kind = "wait"
	case errors.Is(err, jobengine.ErrRefused):
		c.Kind = "refused"
	case errors.Is(err, jobengine.ErrNotSupported):
		c.Kind = "unsupported"
	default:
		c.Kind = "other"
	}
	if err != nil {
		c.Err = err.Error()
		if len(c.Err) > 200 {
			c.Err = c.Err[:200] + "..."
		}
	}
	tr.mu.Lock()
	tr.calls = append(tr.calls, c)
	tr.mu.Unlock()
}

// mark is a position in the record: since counts what came after it.
func (tr *jlTrace) mark() int {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return len(tr.calls)
}

// since counts the calls after the mark for the job (job "" means any) of the operation (op "" means any) that came back as kind (kind "" means any).
func (tr *jlTrace) since(mark int, job, op, kind string) int {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	n := 0
	for i := mark; i < len(tr.calls); i++ {
		c := tr.calls[i]
		if (job == "" || c.Job == job) && (op == "" || c.Op == op) && (kind == "" || c.Kind == kind) {
			n++
		}
	}
	return n
}

// words is the text of the first call after the mark that came back as neither ok nor wait, for a failure message.
func (tr *jlTrace) words(mark int, job string) string {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	for i := mark; i < len(tr.calls); i++ {
		if c := tr.calls[i]; c.Job == job && c.Kind != "ok" && c.Kind != "wait" {
			return fmt.Sprintf("%s %s: %s", c.Op, c.Kind, c.Err)
		}
	}
	return "(none)"
}

type jlTracedModules struct {
	inner jobengine.Modules
	tr    *jlTrace
}

func (m jlTracedModules) For(ctx context.Context, userID, moduleID string) (jobengine.Module, error) {
	mod, err := m.inner.For(ctx, userID, moduleID)
	if err != nil {
		return nil, err
	}
	return jlTracedModule{inner: mod, tr: m.tr}, nil
}

type jlTracedModule struct {
	inner jobengine.Module
	tr    *jlTrace
}

func (m jlTracedModule) Submit(ctx context.Context, j jobs.EngineJob, rows []jobs.EngineRow) (jobengine.SubmitResult, error) {
	res, err := m.inner.Submit(ctx, j, rows)
	m.tr.add("submit", j.ID, err)
	return res, err
}

func (m jlTracedModule) Observe(ctx context.Context, j jobs.EngineJob, fn func([]jobs.ResultItem) error) (jobengine.Observation, error) {
	obs, err := m.inner.Observe(ctx, j, fn)
	m.tr.add("observe", j.ID, err)
	return obs, err
}

func (m jlTracedModule) Cancel(ctx context.Context, j jobs.EngineJob) error {
	err := m.inner.Cancel(ctx, j)
	m.tr.add("cancel", j.ID, err)
	return err
}

func (m jlTracedModule) Pause(ctx context.Context, j jobs.EngineJob) error {
	err := m.inner.Pause(ctx, j)
	m.tr.add("pause", j.ID, err)
	return err
}

func (m jlTracedModule) Resume(ctx context.Context, j jobs.EngineJob) error {
	err := m.inner.Resume(ctx, j)
	m.tr.add("resume", j.ID, err)
	return err
}

// ---- the stack with parameters ---------------------------------------------------------------------------------------------------------------------------------------------

// jlOpts parameterise newJlStackWith.
type jlOpts struct {
	// Cfg builds the configuration the service would read (module URLs and tokens): the URLs may name a forwarder. It receives the module 1 key secret minted for this stack. A module whose URL is empty is NOT
	// configured (its adapter is nil and its jobs wait), as in a service whose dials are unset. Required.
	Cfg func(secret config.Secret) config.Config
	// StaleAfter overrides the staleness dial per module id (the others keep newJlStack's: 30 minutes for module 1, 15 for modules 2 and 3).
	StaleAfter map[string]time.Duration
	// MaxStartDelay is the store's max start delay; 0 means jobs.DefaultMaxStartDelay.
	MaxStartDelay time.Duration
	// SettleEvery is the engine's observation interval; 0 means one second (newJlStack's), a negative value observes a running job at every tick.
	SettleEvery time.Duration
}

// newJlStackWith is newJlStack with its parameters made explicit and the registry's calls recorded (jlTrace). It builds exactly what internal/app builds from a configuration: an adapter per module whose dials
// are set (nil for the others), the module 3 pre-check when module 3 is set, the registry with the module 1 caller ensurer, the engine with engineConfig's batches; the engine serves only the users this stack adopts.
func newJlStackWith(t *testing.T, ctx context.Context, o jlOpts) (*jlStack, *jlTrace) {
	t.Helper()
	if o.Cfg == nil {
		t.Fatal("newJlStackWith: Cfg is required")
	}
	adminURL := requireEnv(t, envAdminURL)
	s := &jlStack{t: t, ctx: ctx, ring: &jlRing{max: 120}}
	s.admin = dial(t, adminURL, func(c *pgx.ConnConfig) { c.RuntimeParams["application_name"] = "jobs-live-test" })
	s.pool = mnyPool(t, adminURL)
	secret := config.Secret(randomHex(t, 32))
	s.cfg = o.Cfg(secret)
	s.cfg.M1KeySecret, s.cfg.M1KeyVersion = secret, config.MinM1KeyVersion
	if s.cfg.ModuleTimeout == 0 {
		s.cfg.ModuleTimeout, s.cfg.ModuleRetries, s.cfg.ModuleRetryBackoff = 20*time.Second, 2, 250*time.Millisecond
	}
	log := slog.New(slog.NewTextHandler(s.ring, &slog.HandlerOptions{Level: slog.LevelDebug}))

	if err := s.admin.QueryRow(ctx, `SELECT id::text FROM platform.admins WHERE disabled_at IS NULL ORDER BY created_at, id LIMIT 1`).Scan(&s.adminID); err != nil {
		t.Fatalf("platform.admins has no active admin (stage 4b creates the first one: every grant and entitlement is written against a named admin): %v", err)
	}
	var err error
	if s.res, err = tenant.NewResolver(s.pool, s.cfg.M1KeySecret, tenant.WithM1KeyVersion(s.cfg.M1KeyVersion)); err != nil {
		t.Fatalf("tenant.NewResolver: %v", err)
	}
	if s.cfg.M1URL != "" {
		if s.fac, err = m1ad.FromConfig(s.cfg); err != nil {
			t.Fatalf("m1.FromConfig: %v", err) // never carries the URL
		}
	}
	var m2ad *m2.Adapter
	switch a, err := m2.FromConfig(s.cfg); {
	case errors.Is(err, m2.ErrNotConfigured):
	case err != nil:
		t.Fatalf("m2.FromConfig: %v", err)
	default:
		m2ad = a
	}
	switch a, err := m3.FromConfig(s.cfg); {
	case errors.Is(err, m3.ErrNotConfigured):
	case err != nil:
		t.Fatalf("m3.FromConfig: %v", err)
	default:
		s.m3ad = a
	}
	s.mny = money.New(s.pool, money.WithLockTimeout(20*time.Second))
	delay := o.MaxStartDelay
	if delay == 0 {
		delay = jobs.DefaultMaxStartDelay
	}
	jopts := []jobs.Option{jobs.WithMaxRows(jobs.DefaultMaxRows), jobs.WithMaxStartDelay(delay), jobs.WithLogger(log), harnessUnsubscribeOption()}
	if s.m3ad != nil {
		jopts = append(jopts, jobs.WithPreChecks(jlM3PreChecks{ad: s.m3ad}))
	}
	if s.store, err = jobs.New(s.pool, s.mny, jopts...); err != nil {
		t.Fatalf("jobs.New: %v", err)
	}
	reg := jobengine.NewRegistry(jobengine.RegistryDeps{
		Resolver:          s.res,
		M1:                s.fac,
		M2:                m2ad,
		M3:                s.m3ad,
		Provisioner:       tenant.NewM1CallerEnsurer(s.res, s.pool),
		SubmitConcurrency: jobengine.DefaultSubmitConcurrency,
		Log:               log,
	})
	s.mine = &jlMine{inner: reg, users: map[string]bool{}}
	s.gate = jobenginetest.NewGate(true)
	tr := &jlTrace{}
	stale := map[string]time.Duration{jobs.ModuleVerify: 30 * time.Minute, jobs.ModuleSendManaged: 15 * time.Minute, jobs.ModuleSendBYO: 15 * time.Minute}
	for k, v := range o.StaleAfter {
		stale[k] = v
	}
	settle := o.SettleEvery
	if settle == 0 {
		settle = time.Second
	}
	s.eng, err = jobengine.New(jobengine.Deps{Store: s.store, Modules: jlTracedModules{inner: s.mine, tr: tr}, Leader: s.gate, Log: log}, jobengine.Config{
		Tick:           700 * time.Millisecond,
		SettleEvery:    settle,
		StaleAfter:     stale,
		SubmitBatch:    500,
		SubmitBatchFor: map[string]int{jobs.ModuleVerify: jobs.MaxEngineIDs},
		StepTimeout:    jobengine.DefaultStepTimeout,
		BackoffCap:     2 * time.Second, // an idle job is looked at again within two seconds: these tests wait for outcomes in seconds
	})
	if err != nil {
		t.Fatalf("jobengine.New: %v", err)
	}
	t.Cleanup(s.cleanup)
	return s, tr
}

// ---- reading what the platform holds, beyond the helpers of jobs_live_helpers_test.go ------------------------------------------------------------------------------------

// jlEvent is one row of platform.job_events.
type jlEvent struct {
	ID                 int64
	Kind, From, To, By string
}

func (s *jlStack) eventsAfter(job string, after int64) []jlEvent {
	s.t.Helper()
	q, err := s.admin.Query(s.ctx, `SELECT id, kind, COALESCE(from_status, ''), COALESCE(to_status, ''), actor_kind FROM platform.job_events WHERE job_id = $1::uuid AND id > $2 ORDER BY id`, job, after)
	if err != nil {
		s.t.Fatalf("read the events of job %s: %v", jlShort(job), err)
	}
	defer q.Close()
	var out []jlEvent
	for q.Next() {
		var e jlEvent
		if err := q.Scan(&e.ID, &e.Kind, &e.From, &e.To, &e.By); err != nil {
			s.t.Fatal(err)
		}
		out = append(out, e)
	}
	if err := q.Err(); err != nil {
		s.t.Fatal(err)
	}
	return out
}

func (s *jlStack) lastEventID(job string) int64 {
	s.t.Helper()
	return s.count("read the newest event of a job", `SELECT COALESCE(max(id), 0) FROM platform.job_events WHERE job_id = $1::uuid`, job)
}

func (s *jlStack) eventKindCount(job, kind string) int64 {
	s.t.Helper()
	return s.count("count the events of a kind", `SELECT count(*) FROM platform.job_events WHERE job_id = $1::uuid AND kind = $2`, job, kind)
}

// resultCount is how many results the job holds, and how many of them are the module's own (origin module).
func (s *jlStack) resultCount(job string) (all, module int64) {
	s.t.Helper()
	if err := s.admin.QueryRow(s.ctx, `SELECT count(*), count(*) FILTER (WHERE origin = 'module') FROM platform.job_results WHERE job_id = $1::uuid`, job).Scan(&all, &module); err != nil {
		s.t.Fatalf("count the results of job %s: %v", jlShort(job), err)
	}
	return all, module
}

// invented is how many results of the job are an outcome the platform made up: a failure of infrastructure or one the charge table could not classify.
func (s *jlStack) invented(job string) int64 {
	s.t.Helper()
	return s.count("count the invented results of a job", `SELECT count(*) FROM platform.job_results WHERE job_id = $1::uuid AND (module_state = 'infrastructure_failure' OR decision = 'unmapped')`, job)
}

func (s *jlStack) chargeRows(job string) int64 {
	s.t.Helper()
	return s.count("count the charge rows of a job", `SELECT count(*) FROM platform.ledger WHERE job_id = $1::uuid AND kind = 'charge'`, job)
}

// booksOf is a fingerprint of everything money for the job: the ledger rows, the charge rows, the reserve and (for a metered module) the user's balance. It must not change while a module cannot be reached.
func (s *jlStack) booksOf(job, user, module string) func() string {
	return func() string {
		rs, ch, rl, ok := s.reserveOf(job)
		av, rv := s.balance(user, module)
		return fmt.Sprintf("ledger rows %d, charge rows %d, reserve (%d, %d, %d, held %v), balance (%d, %d)", s.ledgerRowsOf(job), s.chargeRows(job), rs, ch, rl, ok, av, rv)
	}
}

// ---- holding an outage ---------------------------------------------------------------------------------------------------------------------------------------------------

// jlHold says what one outage must show.
type jlHold struct {
	Job   string
	Link  *jlLink
	Tr    *jlTrace
	Ahead func() (bool, string) // the module's OWN report (read directly, not through the link) has settled more than the platform has recorded: the outage hid something real
	Books func() string
	Diag  func() string
}

// jlHeld is what an outage showed.
type jlHeld struct {
	Held  time.Duration
	Ticks int
	Waits int // observations the engine made of the module that came back as waits
	Stale int // stale events written during the outage
	Ahead string
}

// holdOutage ticks the engine for at least jlOutageHold with the link cut and, at EVERY tick, requires: the link still refuses a request; the job is still running; it gained no result (module or platform) and no
// invented one; the books did not move; the job's events since the cut are `stale` events and nothing else (no move, no flag); the engine's calls to the module came back as waits (never refused, never any other
// error). It ends when the hold is long enough, the module's own report has got ahead of what the platform recorded AND a stale event exists; a hold that reaches jlOutageMaxHold without that fails (the test
// would have measured nothing).
func (s *jlStack) holdOutage(h jlHold) jlHeld {
	s.t.Helper()
	baseAll, baseModule := s.resultCount(h.Job)
	baseBooks := h.Books()
	baseEvent := s.lastEventID(h.Job)
	mark := h.Tr.mark()
	start := time.Now()
	var out jlHeld
	for {
		s.eng.Tick(s.ctx)
		out.Ticks++
		if ok, why := h.Link.reachable(); ok {
			s.t.Fatalf("tick %d of the outage: the link answered a request (%s): the module was reachable again\n%s", out.Ticks, why, s.dump(h.Diag))
		}
		if st := s.status(h.Job); st != jobs.StatusRunning {
			s.t.Fatalf("tick %d of the outage: the job is %s, want running: with the module unreachable nothing may move it\n%s", out.Ticks, st, s.dump(h.Diag))
		}
		if all, mod := s.resultCount(h.Job); all != baseAll || mod != baseModule {
			s.t.Fatalf("tick %d of the outage: the job holds %d result(s) (%d of the module), it held %d (%d) when the link was cut: a result was recorded without the module being heard\n%s", out.Ticks, all, mod, baseAll, baseModule, s.dump(h.Diag))
		}
		if n := s.invented(h.Job); n != 0 {
			s.t.Fatalf("tick %d of the outage: %d result(s) of the job are an infrastructure_failure or unmapped: the engine invented an outcome\n%s", out.Ticks, n, s.dump(h.Diag))
		}
		if got := h.Books(); got != baseBooks {
			s.t.Fatalf("tick %d of the outage: the books moved (was: %s; now: %s): nothing may be charged, reserved or released while the module cannot be heard\n%s", out.Ticks, baseBooks, got, s.dump(h.Diag))
		}
		out.Stale = 0
		for _, ev := range s.eventsAfter(h.Job, baseEvent) {
			if ev.Kind != "stale" {
				s.t.Fatalf("tick %d of the outage: the job gained a %q event (%s -> %s by %s): during an outage the engine may write stale events and nothing else (a failed or expired move, a flag, a record)\n%s", out.Ticks, ev.Kind, jlOrDash(ev.From), jlOrDash(ev.To), ev.By, s.dump(h.Diag))
			}
			out.Stale++
		}
		if n := h.Tr.since(mark, h.Job, "", "other") + h.Tr.since(mark, h.Job, "", "refused"); n != 0 {
			s.t.Fatalf("tick %d of the outage: %d call(s) of the engine to the module came back as something other than a wait; the first: %s\n%s", out.Ticks, n, h.Tr.words(mark, h.Job), s.dump(h.Diag))
		}
		held := time.Since(start)
		ahead, seen := h.Ahead()
		out.Ahead = seen
		if held >= jlOutageHold && ahead && out.Stale >= 1 {
			out.Held = held
			break
		}
		if held >= jlOutageMaxHold {
			s.t.Fatalf("the link was cut for %s and the outage still shows nothing to hold the platform to: the module's own report ahead of the platform: %v (%s); stale events: %d (the dial is %s)\n%s",
				held.Round(time.Second), ahead, seen, out.Stale, jlOutageStale, s.dump(h.Diag))
		}
		select {
		case <-s.ctx.Done():
			s.t.Fatalf("the test's time budget ended during the outage\n%s", s.dump(h.Diag))
		case <-time.After(700 * time.Millisecond):
		}
	}
	out.Waits = h.Tr.since(mark, h.Job, "observe", "wait")
	if out.Waits < 3 {
		s.t.Fatalf("the engine's observations of the module came back as waits only %d time(s) during an outage of %s: it did not try, so the waiting was not measured\n%s", out.Waits, out.Held.Round(time.Second), s.dump(h.Diag))
	}
	return out
}

func jlOrDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// requireMoves fails unless the job moved to each status exactly the given number of times (a status absent from want must be 0 for failed, expired, cancelling and paused).
func (s *jlStack) requireMoves(job string, want map[string]int64) {
	s.t.Helper()
	for _, to := range []string{jobs.StatusConfirmed, jobs.StatusScheduled, jobs.StatusRunning, jobs.StatusCompleting, jobs.StatusSucceeded, jobs.StatusFailed, jobs.StatusExpired, jobs.StatusCancelling, jobs.StatusCancelled, jobs.StatusPaused} {
		if n := s.moves(job, to); n != want[to] {
			s.t.Errorf("the job moved to %s %d time(s), want %d\n%s", to, n, want[to], strings.Join(s.events(job), "\n"))
		}
	}
}

// ---- module 3 --------------------------------------------------------------------------------------------------------------------------------------------------------------

// jlM3OutageItems is the number of recipients of a module 3 outage job; its list also holds two malformed rows and a repeat.
const jlM3OutageItems = 12

// jlRunM3Outage runs one module 3 send job through the engine against the link. With cut set the link is cut after the first results are recorded and held down (holdOutage), then reopened; without it the link
// is never cut and the job must gain results in the same window (the paired control). The tenant has one lane held 1 s after every send and the sink holds every DATA for 1.5 s, so an item takes about three seconds.
func jlRunM3Outage(t *testing.T, e *m3aEnv, s *jlStack, tr *jlTrace, link *jlLink, tn *m3aTenant, cut bool) {
	t.Helper()
	const n = jlM3OutageItems
	what := "outage control (never cut)"
	if cut {
		what = "outage"
	}
	diag := func() string { return e.diagnostics(t, tn) }
	list := jlBuildList(jlM3Addrs(tn, n), 2, 1)
	j, pv, res := s.start(tn.UserID, jobs.CreateReq{Module: jobs.ModuleSendBYO, Rows: list.Rows, Content: jlContent(what)})
	if pv.Rows != n+3 || pv.Pending != n || pv.Malformed != 2 || pv.Duplicates != 1 || pv.Units != n || pv.Metered || res.Job.Status != jobs.StatusConfirmed || res.Pending != n || res.Reserved != 0 {
		t.Fatalf("the preview counts rows %d pending %d malformed %d duplicates %d units %d metered %v and the confirm answered %s with %d pending, %d reserved; want %d, %d, 2, 1, %d, not metered, confirmed, %d, 0",
			pv.Rows, pv.Pending, pv.Malformed, pv.Duplicates, pv.Units, pv.Metered, res.Job.Status, res.Pending, res.Reserved, n+3, n, n, n)
	}
	s.tickToStatus(j.ID, jobs.StatusRunning, 3*time.Minute, diag)

	// the first results are recorded and most of the job is still to come
	s.tickUntil("the engine has recorded the first results of module 3 while most of the job is still to come", 4*time.Minute, 400*time.Millisecond, diag, func() (bool, string) {
		_, mod := s.resultCount(j.ID)
		if mod > n-4 {
			t.Fatalf("module 3 had settled %d of the %d items by the first time the engine recorded any: too fast for a cut in the middle of the job (the sink's hold is 1.5 s and the lane's pause 1 s)\n%s", mod, n, s.dump(diag))
		}
		return mod >= 2, fmt.Sprintf("%d result(s) of the module recorded", mod)
	})
	all0, mod0 := s.resultCount(j.ID)
	if all0 != mod0+3 {
		t.Fatalf("before the cut the job holds %d result(s), %d of them the module's: want the module's plus the platform's three (2 malformed, 1 repeat)", all0, mod0)
	}
	link.requireCarried("before the cut")

	var held jlHeld
	if cut {
		link.requireUp("before the cut")
		link.Cut()
		link.requireDown("just after the cut")
		held = s.holdOutage(jlHold{
			Job: j.ID, Link: link, Tr: tr, Diag: diag,
			Books: s.booksOf(j.ID, tn.UserID, jobs.ModuleSendBYO),
			Ahead: func() (bool, string) {
				rep, err := tn.C.ReportCampaign(s.ctx, j.ID) // directly: the test's own reader never goes through the forwarder
				if err != nil {
					return false, "module 3's own report could not be read directly: " + e.raw.scrub(err.Error())
				}
				return rep.Progress.Terminal > mod0, fmt.Sprintf("module 3's own report says %d terminal item(s), the platform recorded %d", rep.Progress.Terminal, mod0)
			},
		})
		sinkDuring := jlSinkCount(e.sinkView(t), tn.Prefix)
		if int64(sinkDuring) <= mod0 {
			t.Errorf("the sink holds %d message(s) to the tenant at the end of the outage and the platform recorded %d module result(s) when it began: module 3 sent nothing while it was unreachable, so the outage hid nothing (the sink is the independent witness)", sinkDuring, mod0)
		}
		link.mustOpen()
		link.requireUp("after the link was reopened")
		t.Logf("NOTE jl outage m3: the link to module 3 was cut with %d of %d module results recorded; for %s (%d ticks) the job stayed running, gained no result, no charge and no event but %d stale event(s), the engine's %d observation(s) all came back as waits, and the link refused every probe; meanwhile module 3 went on: its own report says %s, the sink holds %d message(s)",
			mod0, n, held.Held.Round(time.Second), held.Ticks, held.Stale, held.Waits, held.Ahead, sinkDuring)
	} else {
		link.requireUp("the control, before its window")
		s.tickUntil("with the link open the platform gains results in the window the outage job gained none in", 90*time.Second, 700*time.Millisecond, diag, func() (bool, string) {
			_, mod := s.resultCount(j.ID)
			return mod > mod0, fmt.Sprintf("%d result(s) of the module recorded, %d at the start of the window", mod, mod0)
		})
	}
	s.tickToStatus(j.ID, jobs.StatusSucceeded, 4*time.Minute, diag)

	// ---- after: the job completed exactly once, and the three places agree ----
	view, sink, rows := s.m3View(e, tn, j.ID)
	if problems := jlM3ThreeWay(view); len(problems) != 0 {
		t.Fatalf("module 3's report, the sink, module 3's own table and the job's results disagree (%d):\n  %s", len(problems), strings.Join(problems, "\n  "))
	}
	by := map[string]int{}
	for _, r := range rows {
		by[r.State]++
	}
	if by["pending"] != n || by["malformed"] != 2 || by["duplicate"] != 1 || len(rows) != n+3 || len(view.Results) != len(rows) || len(view.Report) != n || len(view.Jobs) != n {
		t.Fatalf("the job's rows are %v (%d), it holds %d result(s), module 3's report %d item(s) and its own table %d job(s): want pending %d, malformed 2, duplicate 1, a result per row and %d items and jobs at module 3", by, len(rows), len(view.Results), len(view.Report), len(view.Jobs), n, n)
	}
	if got := jlSinkCount(sink, tn.Prefix); got != n {
		t.Fatalf("the sink holds %d message(s) to the tenant, want %d: every recipient exactly once (the items finished during the outage were sent once, and not again after it)", got, n)
	}
	for _, r := range rows {
		if r.State == "pending" && !r.Submitted {
			t.Errorf("row %d is pending and was never stamped as handed over", r.Ordinal)
		}
	}
	s.requireMoves(j.ID, map[string]int64{jobs.StatusConfirmed: 1, jobs.StatusRunning: 1, jobs.StatusCompleting: 1, jobs.StatusSucceeded: 1})
	for _, c := range []struct{ to, by string }{{jobs.StatusConfirmed, "user"}, {jobs.StatusRunning, "engine"}, {jobs.StatusCompleting, "engine"}, {jobs.StatusSucceeded, "engine"}} {
		if m := s.movesBy(j.ID, c.to, c.by); m != 1 {
			t.Errorf("the job moved to %s %d time(s) by %s, want exactly once by %s", c.to, m, c.by, c.by)
		}
	}
	if f := s.eventKindCount(j.ID, "flag"); f != 0 {
		t.Errorf("the job raised %d flag event(s): nothing may be doubted or invented\n%s", f, strings.Join(s.events(j.ID), "\n"))
	}
	stale := s.eventKindCount(j.ID, "stale")
	switch {
	case cut && stale < 1:
		t.Errorf("the outage job raised no stale event (the dial is %s and the link was down for %s)", jlOutageStale, held.Held.Round(time.Second))
	case !cut && stale != 0:
		t.Errorf("the control job (link never cut, the default staleness dial) raised %d stale event(s): it made progress all along", stale)
	}
	if s.invented(j.ID) != 0 {
		t.Errorf("the job holds an infrastructure_failure or unmapped result")
	}
	if _, _, _, held := s.reserveOf(j.ID); held || s.ledgerRowsOf(j.ID) != 0 {
		t.Errorf("the module 3 job holds a reserve or a ledger row: module 3 is not metered")
	}
	m3aProblems(t, "the books of the user", m3aCompareBooks(e.books(t, tn.UserID)))
	s.requireBooks("after the module 3 " + what + " job")
	e.requireBooks(t, "after the module 3 "+what+" job (the adapter tests' check)")
	if cut {
		t.Logf("NOTE jl outage m3 result: after the link was reopened the job went from %d to %d module results and ended succeeded: completing x1, succeeded x1, no failed move, no flag; %d result(s) for %d row(s); module 3 holds %d job(s) and %d report item(s), the sink %d message(s): each of the %d recipients exactly once", mod0, n, len(view.Results), len(rows), len(view.Jobs), len(view.Report), jlSinkCount(sink, tn.Prefix), n)
	} else {
		t.Logf("NOTE jl outage m3 control: the same job with the link never cut gained results in the window (%d -> more), raised no stale event and ended succeeded the same way: %d result(s) for %d row(s), the sink %d message(s)", mod0, len(view.Results), len(rows), jlSinkCount(sink, tn.Prefix))
	}
}

// TestJobsLive_Outage_M3_HaltsCleanlyAndCompletesOnce (Phase 4, chunk B9, the outage criterion for module 3): a module 3 send job (twelve recipients, two malformed rows and a repeat) is running, some items settled and recorded
// and more in flight, when the platform's path to module 3 is cut (jlLink). For at least 20 seconds of engine ticks, with the link refusing every probe:
//
//	the job stays running; platform.job_results gains NO row (module or platform), the ledger no row, the reserve and the balance do not move; no result is an infrastructure_failure or unmapped
//	the job's events since the cut are stale events and nothing else: no failed move, no flag, no completing; the engine's observations of module 3 all come back as WAITS (never refused, never another error)
//	a `stale` event appears (the dial of this engine is 6 s, the service's is 15 minutes) and the job is STILL running
//	module 3 keeps sending meanwhile: its own report (read directly) holds more final items than the platform recorded, and the sink holds more messages: the outage hid something real
//
// The link is then reopened and the job reaches `succeeded` EXACTLY once: one move to completing and one to succeeded, no failed or expired move, a result for every row (the items module 3 finished while it was
// unreachable are recorded after the recovery, from its report), module 3's own table holds one job per recipient, and the sink holds each recipient once; module 3's report, its table, the sink and the results are
// equal three ways (jlM3ThreeWay).
//
// THE PAIRED CONTROL: the same job for a second user with the link never cut and the DEFAULT staleness dial: the platform gains results in the window the outage job gained none in, no stale event is raised, and the
// job ends in the same way. THE LINK'S OWN CONTROLS: reachable before the cut, refused right after it and at every tick, reachable after Open; the engine's own traffic crossed the forwarder before the cut.
func TestJobsLive_Outage_M3_HaltsCleanlyAndCompletesOnce(t *testing.T) {
	ctx := jlCtx(t, 18*time.Minute)
	e := newM3aEnv(t, ctx)
	e.prepare(t, true, true)
	link := newJlLink(t, "module 3", e.raw.m3URL)
	cfg := func(sec config.Secret) config.Config {
		c := jlConfig(t, sec)
		c.M3URL = link.URL()
		return c
	}
	sOut, trOut := newJlStackWith(t, ctx, jlOpts{Cfg: cfg, StaleAfter: map[string]time.Duration{jobs.ModuleSendBYO: jlOutageStale}})
	sCtl, trCtl := newJlStackWith(t, ctx, jlOpts{Cfg: cfg}) // the default staleness dial: 15 minutes
	// registered after the stacks, so it runs BEFORE their cleanups: a failed test's campaigns are ended through a link that is open
	t.Cleanup(func() {
		if err := link.Open(); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	link.requireUp("at the start")

	k := sOut.m3User(e, "jlout", true)
	c := sCtl.m3User(e, "jlctl", true)
	e.setPacing(t, k, 1, 1, 1)
	e.setPacing(t, c, 1, 1, 1)
	e.setMode(t, m3hMode{DataDelayMs: 1500})

	jlRunM3Outage(t, e, sOut, trOut, link, k, true)
	jlRunM3Outage(t, e, sCtl, trCtl, link, c, false) // the control's recipients carry its own tenant's prefix: the sink's view of it is its own
}

// ---- module 1 --------------------------------------------------------------------------------------------------------------------------------------------------------------

// jlDirectM1 is module 1's front door as the user, at the module's OWN address (not through the forwarder): the test's independent reader of what module 1 did while the platform could not reach it.
func jlDirectM1(t *testing.T, s *jlStack, directURL, user string) *m1ad.Adapter {
	t.Helper()
	tn, err := s.res.ByAuthUser(s.ctx, user)
	if err != nil {
		t.Fatalf("tenant.ByAuthUser: %v", err)
	}
	sc, err := s.res.Scope(s.ctx, tn)
	if err != nil {
		t.Fatalf("tenant.Scope: %v", err)
	}
	fac, err := m1ad.NewFactory(directURL, modclient.FromConfig(s.cfg), nil)
	if err != nil {
		t.Fatalf("m1.NewFactory at the module's own address: %v", err)
	}
	ad, err := fac.ForScope(sc)
	if err != nil {
		t.Fatalf("Factory.ForScope: %v", err) // never carries the key
	}
	return ad
}

// jlRunM1Outage runs one module 1 verify job of `pending` addresses (and two malformed rows and a repeat) through the engine against the link. With cut set the link is cut after the engine recorded the first
// results (and before the job ended) and held down; it returns false, with the job run to its end and nothing asserted about an outage, when module 1 finished the whole job before the cut could land (the
// caller then tries a longer list). Without cut it is the paired control. It returns true when it ran to its end as designed.
func jlRunM1Outage(t *testing.T, s *jlStack, tr *jlTrace, link *jlLink, directURL string, pending int, cut bool) bool {
	t.Helper()
	what, who := "outage control (never cut)", "verify-control"
	if cut {
		what, who = "outage", "verify-outage"
	}
	user := s.newUser(who)
	grant := int64(pending)*3 + 500
	s.grant(user, jobs.ModuleVerify, grant)
	list := jlBuildList(jlAddrs("jlo"+randomHex(t, 3), pending, 4), 2, 1)
	j, pv, res := s.start(user, jobs.CreateReq{Module: jobs.ModuleVerify, Rows: list.Rows})
	if pv.Rows != pending+3 || pv.Pending != pending || pv.Malformed != 2 || pv.Duplicates != 1 || pv.Units != pending || !pv.Metered || pv.UnitCost < 1 || pv.Credits != int64(pending)*int64(pv.UnitCost) || pv.Shortfall != 0 ||
		res.Job.Status != jobs.StatusConfirmed || res.Reserved != pv.Credits || res.Pending != pending {
		t.Fatalf("the preview counts rows %d pending %d malformed %d duplicates %d units %d metered %v credits %d (rate %d) and the confirm answered %s reserved %d pending %d; want %d, %d, 2, 1, %d, metered, units x rate, confirmed, the same credits, %d",
			pv.Rows, pv.Pending, pv.Malformed, pv.Duplicates, pv.Units, pv.Metered, pv.Credits, pv.UnitCost, res.Job.Status, res.Reserved, res.Pending, pending+3, pending, pending, pending)
	}
	diag := s.m1Diag(user, j.ID)
	s.tickToStatus(j.ID, jobs.StatusRunning, 3*time.Minute, diag)
	ref := s.moduleJobRef(j.ID)
	if ref == "" {
		t.Fatal("the job is running but carries no module reference")
	}

	landed := false
	s.tickUntil("the engine has recorded the first results of module 1", 10*time.Minute, 150*time.Millisecond, diag, func() (bool, string) {
		st := s.status(j.ID)
		_, mod := s.resultCount(j.ID)
		if mod >= 1 || st != jobs.StatusRunning {
			landed = st == jobs.StatusRunning && mod >= 1 && mod < int64(pending)
			return true, fmt.Sprintf("status %s, %d result(s) of the module recorded", st, mod)
		}
		return false, fmt.Sprintf("status %s, no result of the module recorded yet", st)
	})
	all0, mod0 := s.resultCount(j.ID)
	if cut && !landed {
		t.Logf("NOTE jl outage m1: with %d addresses module 1 had settled %d of them (status %s) by the time the engine first recorded any: too fast for a cut in the middle; the job is run to its end and a longer list is tried", pending, mod0, s.status(j.ID))
		s.tickToStatus(j.ID, jobs.StatusSucceeded, 10*time.Minute, diag)
		return false
	}
	if all0 != mod0+3 {
		t.Fatalf("before the cut the job holds %d result(s), %d of them the module's: want the module's plus the platform's three (2 malformed, 1 repeat)", all0, mod0)
	}
	link.requireCarried("before the cut")

	var held jlHeld
	if cut {
		direct := jlDirectM1(t, s, directURL, user)
		link.requireUp("before the cut")
		link.Cut()
		link.requireDown("just after the cut")
		held = s.holdOutage(jlHold{
			Job: j.ID, Link: link, Tr: tr, Diag: diag,
			Books: s.booksOf(j.ID, user, jobs.ModuleVerify),
			Ahead: func() (bool, string) {
				st, err := direct.Status(s.ctx, ref) // directly: the test's own reader never goes through the forwarder
				if err != nil {
					return false, fmt.Sprintf("module 1's own status could not be read directly: %v", err)
				}
				return int64(st.Totals.Settled) > mod0, fmt.Sprintf("module 1's own status says %d settled (complete %v), the platform recorded %d", st.Totals.Settled, st.Complete, mod0)
			},
		})
		link.mustOpen()
		link.requireUp("after the link was reopened")
		t.Logf("NOTE jl outage m1: the link to module 1 was cut with %d of %d module results recorded; for %s (%d ticks) the job stayed running, gained no result, no charge and no event but %d stale event(s), the engine's %d observation(s) all came back as waits, and the link refused every probe; meanwhile %s",
			mod0, pending, held.Held.Round(time.Second), held.Ticks, held.Stale, held.Waits, held.Ahead)
	} else {
		link.requireUp("the control, before its window")
		s.tickUntil("with the link open the platform gains results in the window the outage job gained none in", 3*time.Minute, 400*time.Millisecond, diag, func() (bool, string) {
			_, mod := s.resultCount(j.ID)
			st := s.status(j.ID)
			return mod > mod0 || st != jobs.StatusRunning, fmt.Sprintf("status %s, %d result(s) of the module recorded, %d at the start of the window", st, mod, mod0)
		})
	}
	s.tickToStatus(j.ID, jobs.StatusSucceeded, 12*time.Minute, diag)

	// ---- after ----
	final, err := s.store.Get(s.ctx, user, j.ID)
	if err != nil || final.Status != jobs.StatusSucceeded || final.FinishedAt == nil || final.StartedAt == nil {
		t.Fatalf("the job is %+v (error %v): want succeeded with a start and a finish", final, err)
	}
	view := s.m1View(user, j.ID, pending)
	if problems := jlM1ThreeWay(view); len(problems) != 0 {
		if len(problems) > 15 {
			problems = append(problems[:15:15], fmt.Sprintf("... and %d more", len(problems)-15))
		}
		t.Fatalf("the ledger, the results and module 1's own report disagree (%d):\n  %s", len(problems), strings.Join(problems, "\n  "))
	}
	by := map[string]int{}
	for _, r := range view.Rows {
		by[r.State]++
	}
	if by["pending"] != pending || by["malformed"] != 2 || by["duplicate"] != 1 || len(view.Rows) != pending+3 || len(view.Results) != len(view.Rows) || len(view.Report) != pending {
		t.Fatalf("the job's rows are %v (%d), it holds %d result(s) and module 1's report %d row(s): want pending %d, malformed 2, duplicate 1, a result per row and %d rows at module 1", by, len(view.Rows), len(view.Results), len(view.Report), pending, pending)
	}
	mrows, mmal, mrep, owner := s.m1ModuleRows(ref)
	if mrows != int64(pending) || mmal != 0 || mrep != 0 || owner != user {
		t.Fatalf("module 1's own tables hold %d row(s) for the job (%d malformed, %d repeats) owned by %q, want the %d rows the platform sent, no malformed row, no repeat, owned by the user", mrows, mmal, mrep, owner, pending)
	}
	if n := s.count("count module 1's jobs of the user", `SELECT count(*) FROM verify.jobs WHERE owner_user_id = $1::uuid`, user); n != 1 {
		t.Fatalf("module 1 holds %d job(s) for the user, want ONE: the outage must not make the engine submit the job a second time", n)
	}
	// the money
	var wantCharged int64
	for _, m := range view.Report {
		if d := m1bOracle(m); d.Kind == "charge" {
			wantCharged += int64(view.Prices[d.Unit])
		}
	}
	reserved, charged, released, ok := s.reserveOf(j.ID)
	left := pv.Credits - wantCharged
	if !ok || reserved != pv.Credits || charged != wantCharged || released != left || reserved-charged-released != 0 {
		t.Fatalf("the reserve is (reserved %d, charged %d, released %d, exists %v), the report implies charged %d of the %d held (remainder %d): want the whole reserve accounted for, remainder 0", reserved, charged, released, ok, wantCharged, pv.Credits, left)
	}
	wantRel := int64(0)
	if left > 0 {
		wantRel = 1
	}
	if relRows, relCredits := s.releaseRows(j.ID); relRows != wantRel || relCredits != left {
		t.Fatalf("the job has %d release row(s) for %d credits, want %d row(s) for %d (the remainder, once)", relRows, relCredits, wantRel, left)
	}
	if av, rv := s.balance(user, jobs.ModuleVerify); av != grant-wantCharged || rv != 0 {
		t.Fatalf("at the end the balance is available %d reserved %d, want %d and 0 (the grant less what was charged)", av, rv, grant-wantCharged)
	}
	if got := int64(len(view.Ledger)); got != s.chargeRows(j.ID) {
		t.Fatalf("the ledger holds %d charge row(s) of the job, the job's charge rows read %d", s.chargeRows(j.ID), got)
	}
	s.requireMoves(j.ID, map[string]int64{jobs.StatusConfirmed: 1, jobs.StatusRunning: 1, jobs.StatusCompleting: 1, jobs.StatusSucceeded: 1})
	if f := s.eventKindCount(j.ID, "flag"); f != 0 {
		t.Errorf("the job raised %d flag event(s)\n%s", f, strings.Join(s.events(j.ID), "\n"))
	}
	stale := s.eventKindCount(j.ID, "stale")
	switch {
	case cut && stale < 1:
		t.Errorf("the outage job raised no stale event (the dial is %s and the link was down for %s)", jlOutageStale, held.Held.Round(time.Second))
	case !cut && stale != 0:
		t.Errorf("the control job (link never cut, the default staleness dial) raised %d stale event(s)", stale)
	}
	if s.invented(j.ID) != 0 {
		t.Errorf("the job holds an infrastructure_failure or unmapped result")
	}
	if n := s.callerRows(user); n != 1 {
		t.Errorf("module 1's verify.api_callers holds %d row(s) for the user, want 1", n)
	}
	s.requireBooks("after the module 1 " + what + " job")
	if cut {
		t.Logf("NOTE jl outage m1 result: after the link was reopened the job went from %d to %d module results and ended succeeded: completing x1, succeeded x1, no failed move, no flag; the reserve of %d credits was charged %d and the remainder %d released once; module 1 holds ONE job of %d rows", mod0, pending, pv.Credits, wantCharged, left, mrows)
	} else {
		t.Logf("NOTE jl outage m1 control: the same job with the link never cut gained results in the window, raised no stale event and ended succeeded the same way: charged %d of %d credits, remainder %d released", wantCharged, pv.Credits, left)
	}
	return true
}

// jlM1OutageSizes are the lists the module 1 outage tries, shortest first: the cut must land after the first results are recorded and before the job ends, and how fast module 1 settles a list on this
// runner is not known in advance, so a list that module 1 finishes before the engine's first look is run to its end (a good job like any other) and the next, four times longer, is tried. A slow module 1 lands on the first, and the first is short enough to finish in time.
var jlM1OutageSizes = []int{40, 160, 640, 2560, 10240}

// TestJobsLive_Outage_M1_HaltsCleanlyAndCompletesOnce (Phase 4, chunk B9, the outage criterion for module 1; it complements the `jobs` scope's container-stop proof, here with the engine in THIS process and the module
// still running): a verify job is running and the engine has recorded its first results when the platform's path to module 1 is cut. For at least 20 seconds of engine ticks, with the link refusing every probe: the
// job stays running, gains no result and no charge row, the reserve and the balance do not move, no result is invented, the job's events since the cut are stale events and nothing else, every observation the engine
// makes comes back as a wait, a `stale` event appears (6 s here) and the job is STILL running; module 1 meanwhile settles more rows than the platform has recorded (read directly). After the link is reopened the
// job reaches `succeeded` exactly once: one move to completing and one to succeeded, a result for every row, module 1 holds ONE job of exactly the rows the platform sent, the ledger, the results and module 1's own
// report are equal item by item (jlM1ThreeWay), the reserve is charged what the report implies and the remainder released once. THE PAIRED CONTROL is the same job with the link never cut and the default dial.
func TestJobsLive_Outage_M1_HaltsCleanlyAndCompletesOnce(t *testing.T) {
	ctx := jlCtx(t, 25*time.Minute)
	directURL := m3lBaseURL(t, m1bEnvURL)
	link := newJlLink(t, "module 1", directURL)
	cfg := func(sec config.Secret) config.Config {
		c := jlConfig(t, sec)
		c.M1URL = link.URL()
		return c
	}
	sOut, trOut := newJlStackWith(t, ctx, jlOpts{Cfg: cfg, StaleAfter: map[string]time.Duration{jobs.ModuleVerify: jlOutageStale}, SettleEvery: -1})
	sCtl, trCtl := newJlStackWith(t, ctx, jlOpts{Cfg: cfg, SettleEvery: -1})
	t.Cleanup(func() {
		if err := link.Open(); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	link.requireUp("at the start")

	size := 0
	for _, n := range jlM1OutageSizes {
		if jlRunM1Outage(t, sOut, trOut, link, directURL, n, true) {
			size = n
			break
		}
	}
	if size == 0 {
		t.Fatalf("module 1 finished every list it was given (%v addresses) before the engine had recorded a first result and the job was still running: the outage could not be made to land in the middle of a job", jlM1OutageSizes)
	}
	jlRunM1Outage(t, sCtl, trCtl, link, directURL, size, false)
}

// ---- the start deadline: a module that cannot be reached at the start ----------------------------------------------------------------------------------------------------

// jlExpiryWait ticks the engine until the DATABASE's clock is within a second and a half of the job's start deadline, requiring at every tick that the job is still confirmed, nothing was submitted (no row
// stamped, no module reference) and `idle` (what only a started job would have changed at the module) says nothing happened; it returns how many of the engine's Submit calls came back as waits.
func (s *jlStack) jlExpiryWait(job string, tr *jlTrace, link *jlLink, idle func() string, diag func() string) int {
	s.t.Helper()
	mark := tr.mark()
	var deadline time.Time
	if err := s.admin.QueryRow(s.ctx, `SELECT start_deadline FROM platform.jobs WHERE id = $1::uuid`, job).Scan(&deadline); err != nil {
		s.t.Fatalf("read the start deadline: %v", err)
	}
	for ticks := 0; s.dbNow().Add(1500 * time.Millisecond).Before(deadline); ticks++ {
		s.eng.Tick(s.ctx)
		link.requireDown(fmt.Sprintf("tick %d of the wait", ticks+1))
		if st := s.status(job); st != jobs.StatusConfirmed {
			s.t.Fatalf("tick %d, %s before the deadline by the database's clock: the job is %s, want confirmed (it waits, and is expired only AT its deadline)\n%s", ticks+1, deadline.Sub(s.dbNow()).Round(100*time.Millisecond), st, s.dump(diag))
		}
		if stamped := s.count("count the rows handed to a module", `SELECT count(*) FROM platform.job_rows WHERE job_id = $1::uuid AND submitted_at IS NOT NULL`, job); stamped != 0 {
			s.t.Fatalf("%d row(s) of the job are stamped as handed over while the module cannot be reached\n%s", stamped, s.dump(diag))
		}
		if ref := s.moduleJobRef(job); ref != "" {
			s.t.Fatalf("the job carries a module reference while the module cannot be reached\n%s", s.dump(diag))
		}
		if what := idle(); what != "" {
			s.t.Fatalf("while the module cannot be reached: %s\n%s", what, s.dump(diag))
		}
		if n := tr.since(mark, job, "submit", "ok") + tr.since(mark, job, "", "other") + tr.since(mark, job, "", "refused"); n != 0 {
			s.t.Fatalf("tick %d: %d call(s) of the engine to the module came back as ok, refused or another error; the first failure: %s\n%s", ticks+1, n, tr.words(mark, job), s.dump(diag))
		}
		select {
		case <-s.ctx.Done():
			s.t.Fatalf("the test's time budget ended while the job waited")
		case <-time.After(700 * time.Millisecond):
		}
	}
	waits := tr.since(mark, job, "submit", "wait")
	if waits < 3 {
		s.t.Fatalf("the engine's submits came back as waits only %d time(s) before the deadline: it did not try, so the waiting was not measured\n%s", waits, s.dump(diag))
	}
	if st := s.eng.Stats(); st.Refused != 0 {
		s.t.Fatalf("the engine failed %d job(s) because a module refused them: a module that cannot be reached must never fail a job", st.Refused)
	}
	return waits
}

// jlExpired asserts what an expired job leaves: it is expired by the engine at or after its start deadline by the database's clock (and was never failed), every pending row has the platform's own free result
// P-02 `expired` with no unit and every other row keeps the result it was confirmed with, and no module-side event ever happened.
func (s *jlStack) jlExpired(job string) {
	s.t.Helper()
	var afterDeadline bool
	if err := s.admin.QueryRow(s.ctx, `SELECT finished_at IS NOT NULL AND finished_at >= start_deadline FROM platform.jobs WHERE id = $1::uuid`, job).Scan(&afterDeadline); err != nil {
		s.t.Fatalf("read the finish and the deadline: %v", err)
	}
	if !afterDeadline {
		s.t.Errorf("the job was finished before its start deadline by the database's clock (or has no finish time)")
	}
	s.requireMoves(job, map[string]int64{jobs.StatusConfirmed: 1, jobs.StatusExpired: 1})
	if m := s.movesBy(job, jobs.StatusExpired, "engine"); m != 1 {
		s.t.Errorf("the job was expired %d time(s) by the engine, want once", m)
	}
	rows, results := s.rows(job), s.results(job)
	if len(rows) != len(results) {
		s.t.Fatalf("the job has %d row(s) and %d result(s): every row of an expired job has exactly one", len(rows), len(results))
	}
	by := jlByRef(results)
	pending := 0
	for _, row := range rows {
		rs := by[row.ID]
		if len(rs) != 1 {
			s.t.Fatalf("row %d (%s) has %d result(s), want 1", row.Ordinal, row.State, len(rs))
		}
		if row.State == "pending" {
			pending++
			if r := rs[0]; r.Origin != "platform" || r.State != "expired" || r.Decision != "free" || r.Unit != "" || r.Rule != jlPlatformRule("expired") {
				s.t.Errorf("a pending row of the expired job has the result %+v, want the platform's own free `expired` result by P-02, no unit", r)
			}
			continue
		}
		for _, p := range jlPlatformResultProblems(row, rs[0]) {
			s.t.Errorf("%s", p)
		}
	}
	if pending == 0 {
		s.t.Fatalf("the expired job has no pending row: the test's list is wrong")
	}
	if s.invented(job) != 0 {
		s.t.Errorf("the expired job holds an infrastructure_failure or unmapped result")
	}
}

// jlStartedInTime asserts the control job started before its start deadline (the database's clock).
func (s *jlStack) jlStartedInTime(job string) {
	s.t.Helper()
	var ok bool
	if err := s.admin.QueryRow(s.ctx, `SELECT started_at IS NOT NULL AND started_at < start_deadline FROM platform.jobs WHERE id = $1::uuid`, job).Scan(&ok); err != nil {
		s.t.Fatalf("read the start and the deadline: %v", err)
	}
	if !ok {
		s.t.Fatalf("the control job did not start before its start deadline (the link was open: it must)")
	}
}

// TestJobsLive_Expiry_M3_ModuleUnreachableAtStartExpiresAndReleases (Phase 4, chunk B8, the start deadline for module 3): a send job is CONFIRMED (the confirm asks module 3's own quote, so the link is open for it) and the
// platform's path to module 3 is cut before the engine's first tick; the store's max start delay is 10 s (WithMaxStartDelay). Until the deadline, by the DATABASE's clock, every tick of the engine waits: its submits
// come back as waits (never ok, refused or another error), the job stays confirmed, no row is stamped as handed over, the job has no module reference, module 3 holds no job of the user and the sink no message, and the
// engine fails no job. At the deadline the job is EXPIRED by the engine (never before; never failed): every pending row has the platform's own free result P-02, the other rows keep the result they were confirmed
// with, module 3 is not metered so no reserve or ledger row ever existed, the slot is free and balance_drift, reserve_overdrawn and job_settlement_drift are empty.
//
// THE PAIRED CONTROL is the very next job of the same user and module: with the link reopened it CONFIRMS (the slot was free), the engine starts it before its start deadline and it succeeds, three ways equal.
func TestJobsLive_Expiry_M3_ModuleUnreachableAtStartExpiresAndReleases(t *testing.T) {
	ctx := jlCtx(t, 10*time.Minute)
	e := newM3aEnv(t, ctx)
	e.prepare(t, true, true)
	link := newJlLink(t, "module 3", e.raw.m3URL)
	s, tr := newJlStackWith(t, ctx, jlOpts{
		Cfg: func(sec config.Secret) config.Config {
			c := jlConfig(t, sec)
			c.M3URL = link.URL()
			return c
		},
		MaxStartDelay: jlExpiryDelay,
	})
	t.Cleanup(func() {
		if err := link.Open(); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	tn := s.m3User(e, "jlexp", true)
	diag := func() string { return e.diagnostics(t, tn) }

	// ---- the job whose module is unreachable from the start ----
	const n = 6
	list := jlBuildList(jlM3Addrs(tn, n), 1, 1)
	link.requireUp("before the confirm")
	a, pv, res := s.start(tn.UserID, jobs.CreateReq{Module: jobs.ModuleSendBYO, Rows: list.Rows, Content: jlContent("expiry")})
	if pv.Pending != n || pv.Malformed != 1 || pv.Duplicates != 1 || res.Job.Status != jobs.StatusConfirmed || res.Job.StartDeadline == nil || res.Reserved != 0 {
		t.Fatalf("the job's preview counts pending %d malformed %d duplicates %d and the confirm answered %s with a deadline %v and %d reserved; want %d, 1, 1, confirmed, a deadline, 0", pv.Pending, pv.Malformed, pv.Duplicates, res.Job.Status, res.Job.StartDeadline != nil, res.Reserved, n)
	}
	link.Cut()
	link.requireDown("before the engine's first tick")
	waits := s.jlExpiryWait(a.ID, tr, link, func() string {
		if c := e.jobCount(t, tn.UserID, ""); c != 0 {
			return fmt.Sprintf("module 3 holds %d job(s) of the user", c)
		}
		if m := jlSinkCount(e.sinkView(t), tn.Prefix); m != 0 {
			return fmt.Sprintf("the sink holds %d message(s) to the tenant", m)
		}
		if _, _, _, held := s.reserveOf(a.ID); held || s.ledgerRowsOf(a.ID) != 0 {
			return "the job holds a reserve or a ledger row (module 3 is not metered)"
		}
		return ""
	}, diag)
	s.tickToStatus(a.ID, jobs.StatusExpired, 90*time.Second, diag)
	s.jlExpired(a.ID)
	if c := e.jobCount(t, tn.UserID, ""); c != 0 {
		t.Fatalf("module 3 holds %d job(s) of the user after the expiry", c)
	}
	if m := jlSinkCount(e.sinkView(t), tn.Prefix); m != 0 {
		t.Fatalf("the sink holds %d message(s) to the tenant after the expiry", m)
	}
	if av, rv := s.balance(tn.UserID, jobs.ModuleSendBYO); av != 0 || rv != 0 {
		t.Fatalf("the user holds a module 3 balance (available %d reserved %d): module 3 holds no credits", av, rv)
	}
	if st := s.eng.Stats(); st.Refused != 0 {
		t.Fatalf("the engine failed %d job(s) over a module that could not be reached", st.Refused)
	}
	s.requireBooks("after the module 3 job expired")
	t.Logf("NOTE jl expiry m3: the link to module 3 was down from before the first tick; until the start deadline (%s after the confirm, the database's clock) the engine waited (%d submits came back as waits, none ok, refused or other), the job stayed confirmed with no row handed over, no module job and no message at the sink; at the deadline the engine expired it: %d result(s), every pending row P-02 free, no reserve or ledger row (not metered); books closed", jlExpiryDelay, waits, len(s.results(a.ID)))

	// ---- the control: the link open, the same user and module, the same kind of job ----
	if err := link.Open(); err != nil {
		t.Fatal(err)
	}
	link.requireUp("before the control job")
	b, pvB, resB := s.start(tn.UserID, jobs.CreateReq{Module: jobs.ModuleSendBYO, Rows: jlRowsOf(jlM3Addrs(&m3aTenant{Prefix: tn.Prefix + "b"}, n)), Content: jlContent("expiry control")})
	if resB.Job.Status != jobs.StatusConfirmed || pvB.Pending != n {
		t.Fatalf("the control job's confirm answered %s with %d pending, want confirmed and %d: the slot the expired job held must be free", resB.Job.Status, pvB.Pending, n)
	}
	s.tickToStatus(b.ID, jobs.StatusRunning, 90*time.Second, diag)
	s.jlStartedInTime(b.ID)
	s.tickToStatus(b.ID, jobs.StatusSucceeded, 4*time.Minute, diag)
	view, sink, rows := s.m3View(e, tn, b.ID)
	if problems := jlM3ThreeWay(view); len(problems) != 0 {
		t.Fatalf("the control job: module 3's report, the sink, module 3's own table and the results disagree (%d):\n  %s", len(problems), strings.Join(problems, "\n  "))
	}
	if len(rows) != n || len(view.Results) != n || len(view.Jobs) != n || jlSinkCount(sink, tn.Prefix) != n {
		t.Fatalf("the control job holds %d row(s), %d result(s), %d module 3 job(s) and the sink %d message(s): want %d of each", len(rows), len(view.Results), len(view.Jobs), jlSinkCount(sink, tn.Prefix), n)
	}
	s.requireMoves(b.ID, map[string]int64{jobs.StatusConfirmed: 1, jobs.StatusRunning: 1, jobs.StatusCompleting: 1, jobs.StatusSucceeded: 1})
	s.requireBooks("after the module 3 control job")
	t.Logf("NOTE jl expiry m3 control: with the link open the next job of the same user and module confirmed (the slot was free), started before its start deadline and succeeded: %d rows, %d module 3 jobs, %d message(s) at the sink", len(rows), len(view.Jobs), jlSinkCount(sink, tn.Prefix))
}

// TestJobsLive_Expiry_M1_ModuleUnreachableAtStartExpiresAndReleases (Phase 4, chunk B8, the start deadline for module 1): a verify job is confirmed (its reserve held) and the platform's path to module 1 is cut before
// the engine's first tick; the store's max start delay is 10 s. Until the deadline (the database's clock) every tick waits: the job stays confirmed with its reserve and balance as the confirm left them, no row is
// stamped, no module reference exists and module 1 holds no job of the user; the engine fails no job. At the deadline the job is EXPIRED by the engine: the reserve is released whole and once (the balance is
// back to what it was before the confirm, exactly one release row for exactly the reserve, no charge row), every pending row has the platform's own free result P-02, the slot is free and the books are closed.
// THE PAIRED CONTROL is the very next job of the same user: the link reopened, it confirms (the slot was free), is started before its deadline and succeeds, the ledger, the results and module 1's report equal
// item by item.
func TestJobsLive_Expiry_M1_ModuleUnreachableAtStartExpiresAndReleases(t *testing.T) {
	ctx := jlCtx(t, 10*time.Minute)
	directURL := m3lBaseURL(t, m1bEnvURL)
	link := newJlLink(t, "module 1", directURL)
	s, tr := newJlStackWith(t, ctx, jlOpts{
		Cfg: func(sec config.Secret) config.Config {
			c := jlConfig(t, sec)
			c.M1URL = link.URL()
			return c
		},
		MaxStartDelay: jlExpiryDelay,
	})
	t.Cleanup(func() {
		if err := link.Open(); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	user := s.newUser("expiry-m1")
	const grant, n = 500, 8
	s.grant(user, jobs.ModuleVerify, grant)
	diag := func() string { return "" }

	// ---- the job whose module is unreachable from the start ----
	list := jlBuildList(jlAddrs("jle"+randomHex(t, 3), n, 2), 1, 1)
	a, pv, res := s.start(user, jobs.CreateReq{Module: jobs.ModuleVerify, Rows: list.Rows})
	if pv.Pending != n || pv.Malformed != 1 || pv.Duplicates != 1 || !pv.Metered || pv.Credits != int64(n)*int64(pv.UnitCost) || res.Job.Status != jobs.StatusConfirmed || res.Reserved != pv.Credits || res.Job.StartDeadline == nil {
		t.Fatalf("the job's preview counts pending %d malformed %d duplicates %d credits %d and the confirm answered %s reserved %d; want %d, 1, 1, units x rate, confirmed, the same credits", pv.Pending, pv.Malformed, pv.Duplicates, pv.Credits, res.Job.Status, res.Reserved, n)
	}
	reserve := pv.Credits
	if av, rv := s.balance(user, jobs.ModuleVerify); av != grant-reserve || rv != reserve {
		t.Fatalf("after the confirm the balance is available %d reserved %d, want %d and %d", av, rv, grant-reserve, reserve)
	}
	link.Cut()
	link.requireDown("before the engine's first tick")
	waits := s.jlExpiryWait(a.ID, tr, link, func() string {
		if c := s.count("count module 1's jobs of the user", `SELECT count(*) FROM verify.jobs WHERE owner_user_id = $1::uuid`, user); c != 0 {
			return fmt.Sprintf("module 1 holds %d job(s) of the user", c)
		}
		if av, rv := s.balance(user, jobs.ModuleVerify); av != grant-reserve || rv != reserve {
			return fmt.Sprintf("the balance moved to available %d reserved %d, want %d and %d: the reserve is held until the job ends", av, rv, grant-reserve, reserve)
		}
		return ""
	}, diag)
	s.tickToStatus(a.ID, jobs.StatusExpired, 90*time.Second, diag)
	s.jlExpired(a.ID)
	if av, rv := s.balance(user, jobs.ModuleVerify); av != grant || rv != 0 {
		t.Fatalf("after the expiry the balance is available %d reserved %d, want %d and 0: the whole reserve is back", av, rv, grant)
	}
	if rs, ch, rl, ok := s.reserveOf(a.ID); !ok || rs != reserve || ch != 0 || rl != reserve {
		t.Fatalf("the expired job's reserve is (reserved %d, charged %d, released %d, exists %v), want (%d, 0, %d, true)", rs, ch, rl, ok, reserve, reserve)
	}
	if rows, credits := s.releaseRows(a.ID); rows != 1 || credits != reserve {
		t.Fatalf("the expired job has %d release row(s) for %d credits, want exactly one for the whole reserve of %d", rows, credits, reserve)
	}
	if c := s.chargeRows(a.ID); c != 0 {
		t.Fatalf("the expired job has %d charge row(s): nothing was sent", c)
	}
	if c := s.count("count module 1's jobs of the user", `SELECT count(*) FROM verify.jobs WHERE owner_user_id = $1::uuid`, user); c != 0 {
		t.Fatalf("module 1 holds %d job(s) of the user after the expiry", c)
	}
	if st := s.eng.Stats(); st.Refused != 0 {
		t.Fatalf("the engine failed %d job(s) over a module that could not be reached", st.Refused)
	}
	s.requireBooks("after the module 1 job expired")
	t.Logf("NOTE jl expiry m1: the link to module 1 was down from before the first tick; until the start deadline (%s after the confirm, the database's clock) the engine waited (%d submits came back as waits, none ok, refused or other), the job stayed confirmed holding its reserve of %d credits, no row handed over, no module 1 job; at the deadline the engine expired it: %d result(s), every pending row P-02 free, the reserve released whole in exactly one release row, the balance back to %d; books closed", jlExpiryDelay, waits, reserve, len(s.results(a.ID)), grant)

	// ---- the control: the link open, the same user and module ----
	if err := link.Open(); err != nil {
		t.Fatal(err)
	}
	link.requireUp("before the control job")
	list2 := jlBuildList(jlAddrs("jlf"+randomHex(t, 3), n, 2), 1, 1)
	b, pvB, resB := s.start(user, jobs.CreateReq{Module: jobs.ModuleVerify, Rows: list2.Rows})
	if resB.Job.Status != jobs.StatusConfirmed || pvB.Pending != n || resB.Reserved != pvB.Credits {
		t.Fatalf("the control job's confirm answered %s with %d pending and %d reserved, want confirmed, %d and %d: the slot the expired job held must be free", resB.Job.Status, pvB.Pending, resB.Reserved, n, pvB.Credits)
	}
	s.tickToStatus(b.ID, jobs.StatusRunning, 90*time.Second, s.m1Diag(user, b.ID))
	s.jlStartedInTime(b.ID)
	s.tickToStatus(b.ID, jobs.StatusSucceeded, 8*time.Minute, s.m1Diag(user, b.ID))
	view := s.m1View(user, b.ID, n)
	if problems := jlM1ThreeWay(view); len(problems) != 0 {
		t.Fatalf("the control job: the ledger, the results and module 1's report disagree (%d):\n  %s", len(problems), strings.Join(problems, "\n  "))
	}
	if reserved, charged, released, ok := s.reserveOf(b.ID); !ok || reserved-charged-released != 0 {
		t.Fatalf("the control job's reserve is (%d, %d, %d, held %v), want the whole reserve accounted for", reserved, charged, released, ok)
	}
	s.requireMoves(b.ID, map[string]int64{jobs.StatusConfirmed: 1, jobs.StatusRunning: 1, jobs.StatusCompleting: 1, jobs.StatusSucceeded: 1})
	s.requireBooks("after the module 1 control job")
	t.Logf("NOTE jl expiry m1 control: with the link open the next job of the same user and module confirmed (the slot was free), started before its start deadline and succeeded, the ledger, the results and module 1's report equal item by item")
}
