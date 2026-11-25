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

	// The job API's authentication dials are validated here, before anything is opened: an invalid combination is a configuration error (exit status 2), and one log line says which sources are on.
	deps, err := newAPIDeps(cfg, log)
	if err != nil {
		return err
	}
	_ = deps // the job API (Phase 4, chunk B14b) is built from it; until then nothing serves it

	m3, err := newSuspender(cfg)
	if err != nil {
		return err
	}

	pool, err := openPool(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close() // runs after the loops have stopped, and after the leader has returned its connection: see stopLoops below

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

	var draining atomic.Bool
	handler := httpapi.NewHandler(httpapi.Deps{
		Logger:       log,
		Pinger:       scrubbingPinger{pool: pool, dsn: cfg.DatabaseURL},
		Draining:     draining.Load,
		ReadyTimeout: cfg.ReadyzTimeout,
		Now:          time.Now,
	})

	// The startup line names what an operator needs and nothing from the Config struct as a whole.
	log.InfoContext(ctx, "platform starting", "version", Version, "addr", cfg.HTTPAddr, "env", cfg.Env, "instance", instance)

	// The loops start when the server reports it is listening and stop, and are waited for, when Run is
	// leaving, whichever way it leaves: the deferred pool.Close above then closes an idle pool.
	ready := make(chan net.Addr, 1)
	sc := serverConfig(cfg)
	sc.Ready = ready
	stopLoops := runLoops(ctx, ent, ready)

	err = server.Run(ctx, sc, handler, draining.Store, log)
	stopLoops()
	if err != nil {
		log.ErrorContext(ctx, "platform stopped with an error", "error", err.Error())
		return err
	}
	log.InfoContext(ctx, "platform stopped", "reason", "clean drain")
	return nil
}

// runLoops starts the entitlement loops once ready delivers (the HTTP server is listening) and returns the
// function that stops them: it cancels the loops and returns only after they have ended. The loops also end
// when ctx is cancelled, which is the shutdown signal. If the server never becomes ready (it failed to listen)
// the loops never start and the stop function returns at once.
func runLoops(ctx context.Context, ent *entitlements.Service, ready <-chan net.Addr) (stop func()) {
	loopCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-ready:
			_ = ent.Run(loopCtx) // returns nil once both loops have stopped
		case <-loopCtx.Done():
		}
	}()
	return func() {
		cancel()
		<-done
	}
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
