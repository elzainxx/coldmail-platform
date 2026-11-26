package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/adapters/m1"
	"github.com/coldmail-platform/platform/internal/adapters/m3"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi/sendapitest"
	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/entitlements"
	"github.com/coldmail-platform/platform/internal/jobengine"
	"github.com/coldmail-platform/platform/internal/jobs"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// The job engine in the service (Phase 4 B7 to B9; CLAUDE.md: "every dial is seeded with a consumer at the commit that seeds it"). No database is needed here: the pool points at a closed port,
// where no instance can ever take the leader lock, so the engine is built, started, asked its gate and stopped without ever running a step. What a running engine does against a real Postgres and the
// real modules is the harness's (tests/harness).

// ---- the order of the shutdown ---------------------------------------------------------------------------------------------------------------------------------------------------------

// journal is a shared, ordered record of what the fakes did.
type journal struct {
	mu sync.Mutex
	ev []string
}

func (j *journal) add(s string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.ev = append(j.ev, s)
}

func (j *journal) list() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]string(nil), j.ev...)
}

func (j *journal) index(s string) int {
	for i, e := range j.list() {
		if e == s {
			return i
		}
	}
	return -1
}

// fakeRunner is a loopRunner: it records its start, waits for its context to end, takes `winding` to wind down (a step in flight) and records its end (and, for the entitlement service, the
// release of the leader lock, which the real Service.Run does after its loops).
type fakeRunner struct {
	name    string
	j       *journal
	winding time.Duration
	release bool
	started chan struct{}
}

func newFakeRunner(name string, j *journal, winding time.Duration, release bool) *fakeRunner {
	return &fakeRunner{name: name, j: j, winding: winding, release: release, started: make(chan struct{})}
}

func (f *fakeRunner) Run(ctx context.Context) error {
	f.j.add(f.name + " started")
	close(f.started)
	<-ctx.Done()
	time.Sleep(f.winding)
	f.j.add(f.name + " stopped")
	if f.release {
		f.j.add("leader released")
	}
	return nil
}

// shutdownOrderOK says whether the journal shows the contract of runLoops: engine stopped, then the loops stopped, the leader released, and the background function stopped, each only after the
// one before it. It returns the problems.
func shutdownOrderOK(j *journal) []string {
	chain := []string{"engine stopped", "loops stopped", "leader released"}
	var p []string
	for i := range chain {
		if j.index(chain[i]) < 0 {
			p = append(p, chain[i]+" never happened")
		}
	}
	if len(p) > 0 {
		return p
	}
	for i := 1; i < len(chain); i++ {
		if j.index(chain[i]) < j.index(chain[i-1]) {
			p = append(p, chain[i]+" happened before "+chain[i-1])
		}
	}
	if j.index("sync stopped") < j.index("engine stopped") {
		p = append(p, "the background sync stopped before the engine")
	}
	return p
}

func startLoops(t *testing.T, withEngine bool) (stop func(), cancel context.CancelFunc, j *journal, ready chan net.Addr, eng, loops *fakeRunner) {
	t.Helper()
	j = &journal{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ready = make(chan net.Addr, 1)
	loops = newFakeRunner("loops", j, 10*time.Millisecond, true)
	var engine loopRunner
	if withEngine {
		eng = newFakeRunner("engine", j, 60*time.Millisecond, false) // a step in flight takes longer to end than the loops do
		engine = eng
	}
	stop = runLoops(ctx, loops, engine, ready, func(c context.Context) { <-c.Done(); time.Sleep(5 * time.Millisecond); j.add("sync stopped") })
	return stop, cancel, j, ready, eng, loops
}

func TestRunLoops_TheEngineStopsFirstThenTheLoopsAndTheLockIsGivenBackLast(t *testing.T) {
	for _, how := range []string{"stop is called", "the context is cancelled"} {
		t.Run(how, func(t *testing.T) {
			stop, cancel, j, ready, eng, loops := startLoops(t, true)
			ready <- &net.TCPAddr{}
			<-eng.started
			<-loops.started
			if how == "stop is called" {
				stop()
			} else {
				cancel() // the shutdown signal: the sequence starts by itself, and stop joins it
				stop()
			}
			if p := shutdownOrderOK(j); len(p) > 0 {
				t.Fatalf("the shutdown order is wrong: %v\n%v", p, j.list())
			}
			// the engine's wind-down (60 ms) is longer than the loops' (10 ms): had the loops been stopped with it, they would have ended first, and the order above would have said so
			stop() // a second call is a no-op and does not block
		})
	}
}

func TestRunLoops_TheOrderCheckCanFail(t *testing.T) {
	// the control of the test above: a journal in the OLD order (everything cancelled at once, the loops ending first) is reported
	bad := &journal{ev: []string{"loops stopped", "leader released", "engine stopped", "sync stopped"}}
	p := strings.Join(shutdownOrderOK(bad), "; ")
	if !strings.Contains(p, "loops stopped happened before engine stopped") {
		t.Errorf("a journal with the loops before the engine was reported as %q", p)
	}
	if got := shutdownOrderOK(&journal{ev: []string{"engine stopped", "loops stopped"}}); len(got) == 0 {
		t.Error("a journal with no release of the leader lock was accepted")
	}
	good := &journal{ev: []string{"engine stopped", "loops stopped", "leader released", "sync stopped"}}
	if got := shutdownOrderOK(good); len(got) != 0 {
		t.Errorf("the right order was reported as wrong: %v", got)
	}
}

func TestRunLoops_WithNoEngineTheLoopsRunAsBeforeAndNothingStartsBeforeTheServerListens(t *testing.T) {
	// no engine: the loops start on the signal and stop on stop
	stop, _, j, ready, _, loops := startLoops(t, false)
	ready <- &net.TCPAddr{}
	<-loops.started
	stop()
	if j.index("engine started") >= 0 || j.index("loops stopped") < 0 || j.index("leader released") < 0 {
		t.Errorf("with no engine: %v", j.list())
	}
	// the server never listens: nothing starts and stop returns at once
	stop2, _, j2, _, _, _ := startLoops(t, true)
	done := make(chan struct{})
	go func() { stop2(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stop did not return although nothing had started")
	}
	if got := j2.list(); len(got) != 1 || got[0] != "sync stopped" {
		t.Errorf("before the server listened, something started: %v", got) // only the reachability sync runs from the first moment
	}
}

func TestRunLoops_TheSameStartSignalStartsBothAndStopMayBeCalledFromManyGoroutines(t *testing.T) {
	stop, _, j, ready, eng, loops := startLoops(t, true)
	ready <- &net.TCPAddr{}
	<-eng.started
	<-loops.started
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); stop() }()
	}
	wg.Wait()
	if p := shutdownOrderOK(j); len(p) > 0 {
		t.Errorf("%v\n%v", p, j.list())
	}
	if n := countOf(j.list(), "engine stopped"); n != 1 {
		t.Errorf("the engine stopped %d times", n)
	}
}

func countOf(l []string, s string) int {
	n := 0
	for _, e := range l {
		if e == s {
			n++
		}
	}
	return n
}

// ---- the whole service ------------------------------------------------------------------------------------------------------------------------------------------------------------------

func TestRun_TheEngineIsStartedWithTheDialsAndStoppedBeforeTheLoopsAndTheService(t *testing.T) {
	withSuspender(t, &fakeSuspender{}, nil)
	env := merge(base(closedPortDSN(t)), map[string]string{config.VarLogLevel: "debug", config.VarJobTickMS: "400"})
	r := startApp(t, env)
	waitUntil(t, "the engine to start", func() bool { return find(r.logs.lines(t), "job engine started") != nil })
	if l := find(r.logs.lines(t), "job engine started"); l["tick"] != "400ms" {
		t.Errorf("the engine started with tick %v, want PLATFORM_JOB_TICK_MS = 400ms", l["tick"])
	}
	// it asks the ONE election (the loops' own): with no database nobody leads, and it does nothing else
	waitUntil(t, "the election to refuse the lock", func() bool {
		return find(r.logs.lines(t), "loops: the leader lock cannot be tried; this instance idles its loops and tries again at the next tick") != nil
	})
	if err := r.stop(t); err != nil {
		t.Fatalf("the service returned %v after the cancel, want a clean drain", err)
	}
	lines := r.logs.lines(t)
	eng, loops, final := indexOf(lines, "job engine stopped"), indexOf(lines, "entitlement loops stopped"), indexOf(lines, "platform stopped")
	if eng < 0 || loops < 0 || final < 0 || !(eng < loops && loops < final) {
		t.Errorf("the lines must read: job engine stopped (%d), entitlement loops stopped (%d), platform stopped (%d)", eng, loops, final)
	}
	if find(lines, "job engine is switched off") != nil {
		t.Error("the engine was reported switched off while it ran")
	}
	if msg := r.logs.String(); strings.Contains(msg, "goroutine") && strings.Contains(msg, "panic") {
		t.Errorf("the log holds a panic:\n%s", msg)
	}
}

func TestRun_TheSwitchTurnsTheEngineOffAndSaysSoOnce(t *testing.T) {
	withSuspender(t, &fakeSuspender{}, nil)
	r := startApp(t, merge(base(closedPortDSN(t)), map[string]string{config.VarJobEngineEnabled: "false"}))
	waitUntil(t, "the start-up line of the switch", func() bool { return find(r.logs.lines(t), "job engine is switched off") != nil })
	time.Sleep(1200 * time.Millisecond) // long enough for several ticks of an engine, had one been started
	if status, _, _ := fetch(t, "http://"+r.addr+"/healthz"); status != http.StatusOK {
		t.Errorf("healthz = %d with the engine off", status)
	}
	if err := r.stop(t); err != nil {
		t.Fatal(err)
	}
	lines := r.logs.lines(t)
	if n := countLines(lines, "job engine is switched off"); n != 1 {
		t.Errorf("the switch was announced %d times, want once", n)
	}
	if find(lines, "job engine started") != nil || find(lines, "job engine stopped") != nil {
		t.Errorf("an engine ran although it was switched off:\n%s", r.logs.String())
	}
	if l := find(lines, "job engine is switched off"); l["set"] != config.VarJobEngineEnabled {
		t.Errorf("the line names %v, want the variable %s", l["set"], config.VarJobEngineEnabled)
	}
	// the control of this test: the same service with the dial unset runs one (the test above), and an invalid value is a configuration error
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	err := Run(ctx, envOf(merge(base(closedPortDSN(t)), map[string]string{config.VarJobEngineEnabled: "off"})), &syncBuffer{})
	var ce *config.Error
	if !errors.As(err, &ce) || ExitCode(err) != ExitConfig {
		t.Errorf("PLATFORM_JOB_ENGINE_ENABLED=off: Run = %v (exit %d), want a configuration error, exit status 2", err, ExitCode(err))
	}
}

func TestRun_AnEngineBuiltWithoutAnElectionIsNotStarted(t *testing.T) {
	withSuspender(t, &fakeSuspender{}, nil)
	withoutElection(t) // a service with no election has no gate to give the engine: an engine would run in every instance
	r := startApp(t, base(closedPortDSN(t)))
	waitUntil(t, "the line that says why there is no engine", func() bool {
		return find(r.logs.lines(t), "job engine is not started: this service has no leader election, and an engine with no gate would run in every instance") != nil
	})
	if err := r.stop(t); err != nil {
		t.Fatal(err)
	}
	if find(r.logs.lines(t), "job engine started") != nil {
		t.Error("an engine started with no gate")
	}
}

// ---- the dials --------------------------------------------------------------------------------------------------------------------------------------------------------------------------

func loadEngineCfg(t *testing.T, extra map[string]string) config.Config {
	t.Helper()
	cfg, err := config.Load(envOf(merge(base("postgres://platform_app:x@h/db"), extra)))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestEngineConfig_EveryEngineDialReachesTheEngine(t *testing.T) {
	cfg := loadEngineCfg(t, nil)
	got := engineConfig(cfg)
	if got.Tick != 1500*time.Millisecond || got.SettleEvery != 10*time.Second || got.SubmitBatch != 500 || got.StepTimeout != 2*time.Minute ||
		got.StaleAfter[jobs.ModuleVerify] != 30*time.Minute || got.StaleAfter[jobs.ModuleSendManaged] != 15*time.Minute || got.StaleAfter[jobs.ModuleSendBYO] != 15*time.Minute {
		t.Fatalf("the defaults reached the engine as %+v", got)
	}
	if got.SubmitBatchFor[jobs.ModuleVerify] != moduleOneBatch || len(got.SubmitBatchFor) != 1 {
		t.Errorf("module 1's batch is %v, want the one list at %d", got.SubmitBatchFor, moduleOneBatch)
	}
	alt := loadEngineCfg(t, map[string]string{
		config.VarJobTickMS: "400", config.VarJobSettleIntervalSec: "3", config.VarJobStaleAfterMinM1: "61", config.VarJobStaleAfterMinM2: "22", config.VarJobStaleAfterMinM3: "23",
		config.VarJobSubmitBatch: "40", config.VarJobStepTimeoutSec: "45",
	})
	want := got
	want.Tick, want.SettleEvery, want.SubmitBatch, want.StepTimeout = 400*time.Millisecond, 3*time.Second, 40, 45*time.Second
	want.StaleAfter = map[string]time.Duration{jobs.ModuleVerify: 61 * time.Minute, jobs.ModuleSendManaged: 22 * time.Minute, jobs.ModuleSendBYO: 23 * time.Minute}
	if g := engineConfig(alt); !reflect.DeepEqual(g, want) {
		t.Errorf("the dials reached the engine as\n  %+v\nwant\n  %+v", g, want)
	}
	// the control: each dial changes exactly its own field (a loader that crossed two dials would be seen)
	for _, c := range []struct {
		name, value string
		change      func(*jobengine.Config)
	}{
		{config.VarJobTickMS, "400", func(c *jobengine.Config) { c.Tick = 400 * time.Millisecond }},
		{config.VarJobSettleIntervalSec, "3", func(c *jobengine.Config) { c.SettleEvery = 3 * time.Second }},
		{config.VarJobSubmitBatch, "40", func(c *jobengine.Config) { c.SubmitBatch = 40 }},
		{config.VarJobStepTimeoutSec, "45", func(c *jobengine.Config) { c.StepTimeout = 45 * time.Second }},
		{config.VarJobStaleAfterMinM1, "61", func(c *jobengine.Config) { c.StaleAfter[jobs.ModuleVerify] = 61 * time.Minute }},
		{config.VarJobStaleAfterMinM2, "22", func(c *jobengine.Config) { c.StaleAfter[jobs.ModuleSendManaged] = 22 * time.Minute }},
		{config.VarJobStaleAfterMinM3, "23", func(c *jobengine.Config) { c.StaleAfter[jobs.ModuleSendBYO] = 23 * time.Minute }},
	} {
		one := engineConfig(loadEngineCfg(t, map[string]string{c.name: c.value}))
		exp := engineConfig(cfg)
		c.change(&exp)
		if !reflect.DeepEqual(one, exp) {
			t.Errorf("%s alone reached the engine as %+v, want only its own field changed (%+v)", c.name, one, exp)
		}
	}
}

func TestEngine_TheEdgesOfEveryDialAreAcceptedByTheEngineItself(t *testing.T) {
	pool := lazyPool(t)
	log := slog.New(slog.DiscardHandler)
	for name, env := range map[string]map[string]string{
		"the lowest": {config.VarJobTickMS: "100", config.VarJobSettleIntervalSec: "1", config.VarJobStaleAfterMinM1: "1", config.VarJobStaleAfterMinM2: "1", config.VarJobStaleAfterMinM3: "1",
			config.VarJobSubmitBatch: "1", config.VarJobSubmitConcurrency: "1", config.VarJobStepTimeoutSec: "5"},
		"the highest": {config.VarJobTickMS: "60000", config.VarJobSettleIntervalSec: "3600", config.VarJobStaleAfterMinM1: "10080", config.VarJobStaleAfterMinM2: "10080", config.VarJobStaleAfterMinM3: "10080",
			config.VarJobSubmitBatch: "5000", config.VarJobSubmitConcurrency: "64", config.VarJobStepTimeoutSec: "3600"},
	} {
		cfg := loadEngineCfg(t, env)
		deps := apiDeps{}
		if err := deps.build(pool, cfg, log); err != nil {
			t.Fatal(err)
		}
		leader, err := entitlements.NewLeader(pool, "platform-t", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		eng, err := newJobEngine(cfg, deps, leader, log)
		if err != nil || eng == nil {
			t.Errorf("%s: newJobEngine = %v, %v: the engine refused a value its own dial allows", name, eng, err)
		}
	}
}

func TestNewJobEngine_IsNilWhenSwitchedOffOrWithNoElectionAndBuiltOtherwise(t *testing.T) {
	pool := lazyPool(t)
	buf := &syncBuffer{}
	log := slog.New(slog.NewJSONHandler(buf, nil))
	cfg := loadEngineCfg(t, nil)
	deps := apiDeps{}
	if err := deps.build(pool, cfg, log); err != nil {
		t.Fatal(err)
	}
	leader, err := entitlements.NewLeader(pool, "platform-t", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if eng, err := newJobEngine(cfg, deps, leader, log); err != nil || eng == nil {
		t.Fatalf("the control: enabled, with a leader: %v, %v", eng, err)
	}
	off := loadEngineCfg(t, map[string]string{config.VarJobEngineEnabled: "false"})
	if eng, err := newJobEngine(off, deps, leader, log); err != nil || eng != nil {
		t.Errorf("switched off: %v, %v, want no engine and no error", eng, err)
	}
	if eng, err := newJobEngine(cfg, deps, nil, log); err != nil || eng != nil {
		t.Errorf("no election: %v, %v, want no engine and no error", eng, err)
	}
	if eng, err := newJobEngine(cfg, apiDeps{}, leader, log); err == nil || eng != nil {
		t.Errorf("no store and no registry: %v, %v, want an error", eng, err)
	}
	// no row limit the service accepts allows a module 1 job above the engine's batch (TestModuleOneBatch_*), so the start-up has no such job to warn about
	if lines := buf.lines(t); find(lines, "a module 1 job above the engine's batch cannot be handed over in one request: it waits and expires free") != nil {
		t.Errorf("the start-up warns about module 1 jobs above %d rows, which the configuration cannot allow:\n%s", moduleOneBatch, buf.String())
	}
}

// A module 1 job is handed over in ONE request (module 1 deduplicates by the job id and a second request under it is the 409 of a different request), so the engine's batch for module 1 must be at least
// the longest list the service accepts, or a job of that length could never start: it was 20000 against a row limit of 100000. The batch is the store's own limit on one call (jobs.MaxEngineIDs, which
// PendingRows, MarkSubmitted and EnsureModuleItems enforce), the longest list a job may hold (jobs.DefaultMaxRows) and module 1's own limit for one job (m1.MaxRows); the dial that limits the list
// is held to it from the other side: its highest value is the batch, and one more is refused.
func TestModuleOneBatch_CoversTheLargestJobTheConfigurationAcceptsAndIsNoMoreThanTheStoreHandsOut(t *testing.T) {
	batch, perCall, perJob, module := moduleOneBatch, jobs.MaxEngineIDs, jobs.DefaultMaxRows, m1.MaxRows
	if batch != perCall || batch < perJob || batch < module {
		t.Fatalf("module 1's batch is %d, the store's limit on one call %d, a job's longest list %d, module 1's limit for one job %d: the batch must be the call limit and cover both", batch, perCall, perJob, module)
	}
	at := func(rows int) (config.Config, error) {
		return config.Load(envOf(merge(base("postgres://platform_app:x@h/db"), map[string]string{config.VarJobMaxRows: strconv.Itoa(rows)})))
	}
	// the highest the dial accepts is no more than the batch (the engine can hand over every job the service creates) and is the batch itself (nothing is cut off below it)
	cfg, err := at(moduleOneBatch)
	if err != nil || cfg.JobMaxRows != moduleOneBatch {
		t.Fatalf("PLATFORM_JOB_MAX_ROWS=%d: %v (loaded %d); the dial must accept the batch", moduleOneBatch, err, cfg.JobMaxRows)
	}
	_, err = at(moduleOneBatch + 1)
	var ce *config.Error
	if !errors.As(err, &ce) || len(ce.Problems) != 1 || ce.Problems[0].Name != config.VarJobMaxRows {
		t.Errorf("PLATFORM_JOB_MAX_ROWS=%d: %v; the dial must refuse a list the engine could not hand to module 1 in one request", moduleOneBatch+1, err)
	}
	if def := loadEngineCfg(t, nil); def.JobMaxRows > moduleOneBatch || def.JobMaxRows != jobs.DefaultMaxRows {
		t.Errorf("the default row limit is %d, want the store's %d and no more than the batch %d", def.JobMaxRows, jobs.DefaultMaxRows, moduleOneBatch)
	}
	// the engine is configured with it for module 1 and for no other module (the senders offer rows in the small batches of PLATFORM_JOB_SUBMIT_BATCH)
	got := engineConfig(cfg)
	if got.SubmitBatchFor[jobs.ModuleVerify] != moduleOneBatch || len(got.SubmitBatchFor) != 1 || got.SubmitBatch >= moduleOneBatch {
		t.Errorf("the engine's batches are %v and %d", got.SubmitBatchFor, got.SubmitBatch)
	}
}

func TestRegistry_TheSubmitConcurrencyDialReachesTheRegistryAndTheAdaptersAreBuiltFromTheirDials(t *testing.T) {
	pool := lazyPool(t)
	log := slog.New(slog.DiscardHandler)
	// module 3 and nothing else is configured: its adapter is built, the other two are nil
	var inFlight, maxIn atomic.Int64
	stub := sendapitest.NewStub(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			m := maxIn.Load()
			if n <= m || maxIn.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
		sendapitest.JSON(w, 202, `{"job_id":"0d3e5f70-8192-4a3b-8c4d-000000000001","state":"queued"}`)
	})
	token := sendapitest.Token(t)
	for _, c := range []struct {
		dial string
		want int64
	}{{"3", 3}, {"6", 6}} {
		env := map[string]string{config.VarM3URL: stub.URL(), config.VarM3ServiceToken: token, config.VarJobSubmitConcurrency: c.dial}
		cfg := loadEngineCfg(t, env)
		ad, err := newModuleAdapters(cfg, log)
		if err != nil || ad.M3 == nil || ad.M1 != nil || ad.M2 != nil {
			t.Fatalf("adapters = %+v, %v; want only module 3", ad, err)
		}
		reg := newModuleRegistry(pool, cfg, ad, nil, log)
		mod, err := reg.For(t.Context(), "2f6d1c1e-7a4b-4c55-9f0e-5b7a3c9d8e21", jobs.ModuleSendBYO)
		if err != nil {
			t.Fatal(err)
		}
		rows := make([]jobs.EngineRow, 30)
		for i := range rows {
			rows[i] = jobs.EngineRow{ID: fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1), Ordinal: i + 1}
		}
		maxIn.Store(0)
		res, err := mod.Submit(t.Context(), jobs.EngineJob{Job: jobs.Job{ID: "5a1c9e30-7d2b-4f6a-9b8e-1c3d5e7f9a02", Module: jobs.ModuleSendBYO}, Pending: len(rows)}, rows)
		if err != nil || len(res.Accepted) != len(rows) {
			t.Fatalf("dial %s: Submit = %d, %v", c.dial, len(res.Accepted), err)
		}
		if got := maxIn.Load(); got != c.want {
			t.Errorf("PLATFORM_JOB_SUBMIT_CONCURRENCY=%s: %d run calls were in flight together, want %d", c.dial, got, c.want)
		}
	}
	// the adapters whose dials are unset are nil, and a module that is not configured waits (never fails the job)
	cfg := loadEngineCfg(t, nil)
	ad, err := newModuleAdapters(cfg, log)
	if err != nil || ad.M1 != nil || ad.M2 != nil || ad.M3 != nil {
		t.Fatalf("with no module dial set the adapters are %+v, %v; want none", ad, err)
	}
	// every dial set: all three are built
	all := loadEngineCfg(t, map[string]string{config.VarM1URL: "http://m1.test:8080", config.VarM2URL: "http://m2.test:8080", config.VarM2ServiceToken: token, config.VarM3ServiceToken: token})
	if ad, err := newModuleAdapters(all, log); err != nil || ad.M1 == nil || ad.M2 == nil || ad.M3 == nil {
		t.Errorf("with every dial set the adapters are %+v, %v; want all three", ad, err)
	}
}

func TestNewModuleAdapters_AWrongConfigurationIsAConfigErrorNamingTheVariable(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	token := sendapitest.Token(t)
	// module 3's service token must not be the operator key: the two domains never share a credential
	same := loadEngineCfg(t, map[string]string{config.VarM3ServiceToken: m3Env()[config.VarM3AdminToken]})
	_, err := newModuleAdapters(same, log)
	var ce *config.Error
	if !errors.As(err, &ce) || ce.Problems[0].Name != config.VarM3ServiceToken || ExitCode(err) != ExitConfig {
		t.Errorf("a shared credential: %v (exit %d), want a configuration error naming %s", err, ExitCode(err), config.VarM3ServiceToken)
	}
	if err != nil && strings.Contains(err.Error(), m3Env()[config.VarM3AdminToken]) {
		t.Errorf("the error carries the credential: %v", err)
	}
	// the control: a different token is accepted
	if _, err := newModuleAdapters(loadEngineCfg(t, map[string]string{config.VarM3ServiceToken: token}), log); err != nil {
		t.Errorf("a distinct service token: %v", err)
	}
}

// ---- the module 3 pre-check of a confirm ------------------------------------------------------------------------------------------------------------------------------------------

func fixtureM3(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "adapters", "m3", "testdata", "contract", name))
	if err != nil {
		t.Fatalf("the fixture %s cannot be read: %v", name, err)
	}
	return b
}

func preChecksOver(t *testing.T, h func(w http.ResponseWriter, r *http.Request, n int), opts modclient.Options) (m3PreChecks, *sendapitest.Stub) {
	t.Helper()
	stub := sendapitest.NewStub(t, h)
	ad, err := m3.NewAdapter(sendapitest.ModClient(t, modclient.M3, stub.URL(), sendapitest.Token(t), opts))
	if err != nil {
		t.Fatal(err)
	}
	return m3PreChecks{ad: ad}, stub
}

const (
	preUser     = "2f6d1c1e-7a4b-4c55-9f0e-5b7a3c9d8e21"
	preCampaign = "5a1c9e30-7d2b-4f6a-9b8e-1c3d5e7f9a02"
)

func TestM3PreChecks_FeasibleInfeasibleWithItsCausesAndUnavailable(t *testing.T) {
	feasible, infeasible := fixtureM3(t, "quote.feasible.json"), fixtureM3(t, "quote.infeasible.json")
	// the module's own answers: the recorded feasible quote (for 100 recipients) and the recorded infeasible one (queue cap, for 1)
	p, stub := preChecksOver(t, sendapitest.Serve(200, feasible), modclient.Options{})
	f, err := p.M3Feasibility(t.Context(), preUser, preCampaign, 100)
	if err != nil || !f.Feasible || f.Unavailable || len(f.Causes) != 0 {
		t.Errorf("a feasible quote: %+v, %v", f, err)
	}
	if r := stub.Requests(); len(r) != 1 || !strings.Contains(string(r[0].Body), preCampaign) || !strings.Contains(string(r[0].Body), preUser) {
		t.Errorf("the quote did not name the campaign and the tenant: %v", r)
	}
	p, _ = preChecksOver(t, sendapitest.Serve(200, infeasible), modclient.Options{})
	f, err = p.M3Feasibility(t.Context(), preUser, preCampaign, 1)
	if err != nil || f.Feasible || f.Unavailable || !reflect.DeepEqual(f.Causes, []string{"queue_cap"}) {
		t.Errorf("an infeasible quote: %+v, %v; want infeasible with the cause queue_cap", f, err)
	}

	// a module that cannot be asked now is Unavailable, never feasible and never an error
	for name, c := range map[string]struct {
		h    func(w http.ResponseWriter, r *http.Request, n int)
		opts modclient.Options
	}{
		"503 unavailable": {func(w http.ResponseWriter, _ *http.Request, _ int) {
			sendapitest.JSON(w, 503, sendapitest.Envelope("unavailable"))
		}, modclient.Options{}},
		"503 paused": {func(w http.ResponseWriter, _ *http.Request, _ int) {
			sendapitest.JSON(w, 503, sendapitest.Envelope("paused"))
		}, modclient.Options{}},
		"429 rate limited": {func(w http.ResponseWriter, _ *http.Request, _ int) {
			sendapitest.JSON(w, 429, sendapitest.Envelope("rate_limited"))
		}, modclient.Options{}},
		"a timeout": {func(w http.ResponseWriter, _ *http.Request, _ int) {
			time.Sleep(300 * time.Millisecond)
			sendapitest.JSON(w, 200, string(feasible))
		}, modclient.Options{Timeout: 100 * time.Millisecond}},
	} {
		pu, _ := preChecksOver(t, c.h, c.opts)
		f, err := pu.M3Feasibility(t.Context(), preUser, preCampaign, 100)
		if err != nil || !f.Unavailable || f.Feasible {
			t.Errorf("%s: %+v, %v; want Unavailable and no error", name, f, err)
		}
	}
	// an answer the platform cannot use is an ERROR (the store refuses the confirm with it and holds no credits), not "unavailable" and not "feasible"
	for name, h := range map[string]func(w http.ResponseWriter, r *http.Request, n int){
		"500": func(w http.ResponseWriter, _ *http.Request, _ int) {
			sendapitest.JSON(w, 500, sendapitest.Envelope("internal_error"))
		},
		"400": func(w http.ResponseWriter, _ *http.Request, _ int) {
			sendapitest.JSON(w, 400, sendapitest.Envelope("validation_failed"))
		},
		"a body that lies": sendapitest.Serve(200, []byte(`{"feasible":true}`)),
	} {
		pe, _ := preChecksOver(t, h, modclient.Options{})
		f, err := pe.M3Feasibility(t.Context(), preUser, preCampaign, 100)
		if err == nil || f.Feasible || f.Unavailable {
			t.Errorf("%s: %+v, %v; want an error and nothing believed", name, f, err)
		}
	}
	// a user id that is no tenant is refused before any request
	pb, stub2 := preChecksOver(t, sendapitest.Serve(200, feasible), modclient.Options{})
	if _, err := pb.M3Feasibility(t.Context(), "not-a-uuid", preCampaign, 100); err == nil || stub2.Count() != 0 {
		t.Errorf("a bad user id: %v, %d request(s)", err, stub2.Count())
	}
}

func TestNewModuleAdapters_Module3IsWiredIntoTheStoreOnlyWhenItIsConfigured(t *testing.T) {
	pool := lazyPool(t)
	log := slog.New(slog.DiscardHandler)
	token := sendapitest.Token(t)
	with := loadEngineCfg(t, map[string]string{config.VarM3ServiceToken: token})
	d := apiDeps{}
	if err := d.build(pool, with, log); err != nil {
		t.Fatal(err)
	}
	if d.Adapters.M3 == nil || d.Registry == nil || d.Jobs == nil {
		t.Fatalf("build with module 3 configured: adapters %+v registry %v store %v", d.Adapters, d.Registry != nil, d.Jobs != nil)
	}
	if got, ok := d.PreChecks.(m3PreChecks); !ok || got.ad != d.Adapters.M3 {
		t.Errorf("module 3 is configured and the store was given %T, want the pre-check over the module 3 adapter", d.PreChecks)
	}
	// the control: nothing configured, no pre-check (a nil PreChecks is "none", never a typed nil)
	d2 := apiDeps{}
	if err := d2.build(pool, loadEngineCfg(t, nil), log); err != nil {
		t.Fatal(err)
	}
	if d2.PreChecks != nil {
		t.Errorf("module 3 is not configured and the store has a pre-check: %T", d2.PreChecks)
	}
}
