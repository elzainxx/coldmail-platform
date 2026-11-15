// Package app wires the platform service: configuration, logger, database pool, handler and server. The
// process entry point (cmd/platform) is a thin shell around Run, so everything with behaviour is here,
// where it can be tested over a real listener.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/dsnscrub"
	"github.com/coldmail-platform/platform/internal/httpapi"
	"github.com/coldmail-platform/platform/internal/logging"
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

	pool, err := openPool(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	var draining atomic.Bool
	handler := httpapi.NewHandler(httpapi.Deps{
		Logger:       log,
		Pinger:       scrubbingPinger{pool: pool, dsn: cfg.DatabaseURL},
		Draining:     draining.Load,
		ReadyTimeout: cfg.ReadyzTimeout,
		Now:          time.Now,
	})

	// The startup line names what an operator needs and nothing from the Config struct as a whole.
	log.InfoContext(ctx, "platform starting", "version", Version, "addr", cfg.HTTPAddr, "env", cfg.Env)

	if err := server.Run(ctx, serverConfig(cfg), handler, draining.Store, log); err != nil {
		log.ErrorContext(ctx, "platform stopped with an error", "error", err.Error())
		return err
	}
	log.InfoContext(ctx, "platform stopped", "reason", "clean drain")
	return nil
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
