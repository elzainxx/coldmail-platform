// Package app wires the platform service: configuration, logger, database pool, handler, server and the two
// entitlement loops. The process entry point (cmd/platform) is a thin shell around Run, so everything with
// behaviour is here, where it can be tested over a real listener.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/dsnscrub"
	"github.com/coldmail-platform/platform/internal/entitlements"
	"github.com/coldmail-platform/platform/internal/httpapi"
	"github.com/coldmail-platform/platform/internal/logging"
	"github.com/coldmail-platform/platform/internal/m3admin"
	"github.com/coldmail-platform/platform/internal/server"
)

// Version is reported in the startup log line. A release build sets it with
// -ldflags "-X github.com/coldmail-platform/platform/internal/app.Version=<version>".
var Version = "dev"

// Exit codes of the process.
const (
	ExitOK      = 0
	ExitFailure = 1 // the service failed while running or could not start for a reason other than configuration
	ExitConfig  = 2 // the configuration is invalid; the message names variables and reasons, never values
)

// ExitCode maps the error Run returned to the process exit status: 0 after a clean drain, 2 for invalid
// configuration, 1 for anything else.
func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	var ce *config.Error
	if errors.As(err, &ce) {
		return ExitConfig
	}
	return ExitFailure
}

// Run configures and serves the platform until ctx is cancelled, then drains. It returns nil after a
// clean drain. Logs go to logw as JSON.
//
// The database pool is lazy: Run does not need the database to be reachable to start. /healthz answers
// from the first moment and /readyz reports the database truthfully, so a database outage leaves the
// process alive and not ready, which is what an orchestrator needs to see.
//
// Once the HTTP server is listening, the two entitlement loops start (expiry of module 3 entitlements, and
// reconciliation of module 3's suspend state). They run only in the instance that holds the leader advisory
// lock: a second instance still serves HTTP but idles its loops and takes over when the leader's database
// session ends. On shutdown they are cancelled and WAITED FOR, and the leader gives its lock and its
// connection back, before the pool closes, so no pass is writing to a closed pool and the pool is not held
// open by a connection nobody returns. A failed pass never ends a loop, and neither loop changes what
// /healthz and /readyz say.
//
// The job engine (internal/jobengine, jobengine.go) runs beside them, in the same instance, under the SAME election (entitlements.Service.Leader): only the leader runs it. On shutdown it is stopped
// FIRST and waited for, then the entitlement loops (whose Run gives the leader lock and its connection back), then the pool closes: an engine step that is running when the loops stop would meet a
// released lock and a closing pool. PLATFORM_JOB_ENGINE_ENABLED=false builds none.
//
// Beside them runs the reachability sync (reachability.go): from the moment the pool exists, in every instance, it reads module 1's describe (vendor_fallback.reachable) and writes the answer into
// platform.unit_costs (at start, then every minute; every 5 seconds until the first success); a describe it cannot read writes nothing. It runs only where module 1 can be asked (PLATFORM_M1_URL and
// PLATFORM_M1_KEY_SECRET set). It is stopped and waited for by the same stop function, before the pool closes.
//
// No returned error contains the database URL: parse and pool errors pass through dsnscrub, and a
// configuration error names variables and reasons only.
func Run(ctx context.Context, getenv func(string) string, logw io.Writer) error {
	cfg, err := config.Load(getenv)
	if err != nil {
		return err
	}
	log, err := logging.New(cfg.LogLevel, logw)
	if err != nil {
		return fmt.Errorf("build logger: %w", err)
	}
	// a variable that was removed and is still in the environment is ignored, and said so by name (never its value): a setting that looks live and is not must not pass in silence
	for _, r := range config.SetRemoved(getenv) {
		log.Warn("a removed configuration variable is set and is ignored", "variable", r.Name, "instead", r.Instead)
	}

	// The job API's authentication dials are validated here, before anything is opened: an invalid combination is a configuration error (exit status 2), and one log line says which sources are on.
	deps, err := newAPIDeps(cfg, log)
	if err != nil {
		return err
	}

	m3, err := newSuspender(cfg)
	if err != nil {
		return err
	}

	pool, err := openPool(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close() // runs after the loops (the entitlement loops and the reachability sync) have stopped, and after the leader has returned its connection: see stopLoops below

	// THE money store, the job store and the tenant resolver, built once over the pool and shared by everything that needs them (Phase 4 B14b). The job API is built here too, only when a key
	// source is set; without one its routes are not mounted.
	if err := deps.build(pool, cfg, log); err != nil {
		return err
	}

	instance, err := entitlements.ResolveInstanceID(cfg.InstanceID)
	if err != nil {
		return fmt.Errorf("resolve this instance's name: %w", err)
	}
	ent, err := entitlements.New(entitlements.Config{
		Pool:           pool,
		M3:             m3,
		Log:            log,
		ExpireEvery:    cfg.EntitlementExpireInterval,
		ReconcileEvery: cfg.M3ReconcileInterval,
		ReverifyEvery:  cfg.M3ReverifyInterval,
		Redact:         []string{cfg.M3AdminToken.Reveal()},
		InstanceID:     instance,
	}, entitlementOptions()...)
	if err != nil {
		return fmt.Errorf("build the entitlement loops: %w", err)
	}

	eng, err := newJobEngine(cfg, deps, ent.Leader(), log)
	if err != nil {
		return err
	}

	var draining atomic.Bool
	handler := httpapi.NewHandler(httpapi.Deps{
		Logger:       log,
		Pinger:       scrubbingPinger{pool: pool, dsn: cfg.DatabaseURL},
		Draining:     draining.Load,
		ReadyTimeout: cfg.ReadyzTimeout,
		Now:          time.Now,
		Mount:        deps.mount(), // the job API's routes, or nil: then every one of them is the catch-all's 404
	})

	// The startup line names what an operator needs and nothing from the Config struct as a whole.
	log.InfoContext(ctx, "platform starting", "version", Version, "addr", cfg.HTTPAddr, "env", cfg.Env, "instance", instance)

	// The loops start when the server reports it is listening and stop, and are waited for, when Run is
	// leaving, whichever way it leaves: the deferred pool.Close above then closes an idle pool.
	ready := make(chan net.Addr, 1)
	sc := serverConfig(cfg)
	sc.Ready = ready
	// The reachability sync (Phase 4, chunk B3) is not a leader's loop: it runs in EVERY instance, from the start and whether or not the server is listening yet, and it stops with the others.
	var background []func(context.Context)
	if deps.Vendor != nil {
		reach := newReachabilitySetter(pool, deps.Money, cfg)
		background = append(background, func(loopCtx context.Context) {
			syncReachability(loopCtx, deps.Vendor, reach, log, sleepContext)
		})
	} else {
		log.Info("the reachability sync is not started: module 1 cannot be asked in this process", "set", config.VarM1URL+" and "+config.VarM1KeySecret, "effect", "the price of a module 1 job follows the flag stored in the database")
	}
	var engine loopRunner // a nil *jobengine.Engine must not become a non-nil interface
	if eng != nil {
		engine = eng
	}
	stopLoops := runLoops(ctx, ent, engine, ready, background...)

	err = server.Run(ctx, sc, handler, draining.Store, log)
	stopLoops()
	if err != nil {
		log.ErrorContext(ctx, "platform stopped with an error", "error", err.Error())
		return err
	}
	log.InfoContext(ctx, "platform stopped", "reason", "clean drain")
	return nil
}

// loopRunner is what runLoops runs: the entitlement service and the job engine (a fake of each in the test of the shutdown order).
type loopRunner interface {
	Run(ctx context.Context) error
}

// runLoops starts the entitlement loops, and the job engine when there is one, once ready delivers (the HTTP server is listening), and returns the function that stops them. It is the ONE
// shutdown path of everything that runs against the pool, and its ORDER is the contract:
//
//  1. the job engine stops first and is WAITED FOR (its context ends; the step it is in ends with it);
//  2. then the entitlement loops stop and are waited for (their Run gives the leader lock and its connection back, and the lock is the engine's gate: nothing is left asking a lock that is gone);
//  3. the functions of background (the reachability sync) stop with the loops;
//
// and the caller closes the pool after the stop function returned. The same sequence starts when ctx is cancelled (the shutdown signal), whoever calls stop: the loops' own context is NOT
// derived from ctx's cancellation, it ends only when the engine has ended. If the server never becomes ready (it failed to listen) nothing starts and the stop function returns at once. The stop
// function is safe to call more than once and from several goroutines.
//
// Each function of background starts at once, in a goroutine of its own, and runs until the loops' context is done (the reachability sync).
func runLoops(ctx context.Context, ent loopRunner, engine loopRunner, ready <-chan net.Addr, background ...func(context.Context)) (stop func()) {
	engCtx, engCancel := context.WithCancel(ctx)
	loopCtx, loopCancel := context.WithCancel(context.WithoutCancel(ctx))
	var engWG, wg sync.WaitGroup

	// ready carries one value: it is turned into a signal that every starter can see
	listening := make(chan struct{})
	go func() {
		select {
		case <-ready:
			close(listening)
		case <-engCtx.Done():
		}
	}()
	if engine != nil {
		engWG.Add(1)
		go func() {
			defer engWG.Done()
			select {
			case <-listening:
				_ = engine.Run(engCtx) // returns nil once the engine has stopped
			case <-engCtx.Done():
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		select {
		case <-listening:
			_ = ent.Run(loopCtx) // returns nil once both loops have stopped and the leader lock is given back
		case <-loopCtx.Done():
		}
	}()
	for _, f := range background {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f(loopCtx)
		}()
	}

	var once sync.Once
	done := make(chan struct{})
	shutdown := func() {
		once.Do(func() {
			engCancel()
			engWG.Wait() // the engine has stopped: nothing asks the leader lock any more
			loopCancel()
			wg.Wait() // the entitlement loops have stopped and have released the lock; the reachability sync has stopped
			close(done)
		})
		<-done
	}
	go func() { // the shutdown signal starts the sequence, whoever calls stop
		select {
		case <-ctx.Done():
			shutdown()
		case <-done:
		}
	}()
	return shutdown
}

// entitlementOptions is the list of options the entitlement service is built with. The service passes NONE: leader election is on, so the two loops run only in the instance that holds the leader
// lock (Docs/install.md, "Running more than one instance"). It is a variable only so that a test of the loops' own rhythm, which runs against a closed port where no instance could ever become the
// leader, can switch the election off; nothing else assigns it, and TestLeader_NoProductionPathChangesTheWiring (internal/entitlements) fails if non-test code names an option.
var entitlementOptions = func() []entitlements.Option { return nil }

// newSuspender builds the module 3 admin client from the configuration. It is a variable so that a test can
// supply a fake; nothing else assigns it. The client's HTTP side follows the platform's house style (the probe
// client): the per-request timeout of PLATFORM_M3_REQUEST_TIMEOUT_SEC, no redirect followed, no proxy, and a
// fresh connection per request.
var newSuspender = func(cfg config.Config) (entitlements.Suspender, error) {
	c, err := m3admin.New(cfg.M3URL, cfg.M3AdminToken, newM3HTTPClient(cfg.M3RequestTimeout))
	if err != nil {
		// m3admin's errors never contain the URL or the key, so this text is safe to return and print.
		return nil, fmt.Errorf("build the module 3 client: %w", err)
	}
	return c, nil
}

// newM3HTTPClient is the transport of the module 3 client.
func newM3HTTPClient(timeout time.Duration) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.DisableKeepAlives = true
	return &http.Client{
		Timeout:       timeout,
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// serverConfig is where the HTTP-server dials meet their consumer.
func serverConfig(cfg config.Config) server.ServerConfig {
	return server.ServerConfig{
		Addr:              cfg.HTTPAddr,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ShutdownTimeout:   cfg.ShutdownTimeout,
	}
}

// openPool parses the connection string and creates the pool WITHOUT connecting (pgxpool opens
// connections on demand; MinConns is 0). A string pgx cannot parse is reported as a configuration error
// on PLATFORM_DATABASE_URL, with the driver's message scrubbed of the string and its credentials.
func openPool(ctx context.Context, dsn config.Secret) (*pgxpool.Pool, error) {
	pc, err := pgxpool.ParseConfig(dsn.Reveal())
	if err != nil {
		return nil, &config.Error{Problems: []config.Problem{{
			Name:   config.VarDatabaseURL,
			Reason: "is not a usable connection string: " + dsnscrub.Scrub(err, dsn.Reveal()).Error(),
		}}}
	}
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("create database pool: %w", dsnscrub.Scrub(err, dsn.Reveal()))
	}
	return pool, nil
}

// scrubbingPinger is the readiness dependency: the pool, with every error passed through dsnscrub so that
// nothing downstream (the handler, a future caller that logs the error) can ever hold an unsafe value.
type scrubbingPinger struct {
	pool *pgxpool.Pool
	dsn  config.Secret
}

func (p scrubbingPinger) Ping(ctx context.Context) error {
	return dsnscrub.Scrub(p.pool.Ping(ctx), p.dsn.Reveal())
}
