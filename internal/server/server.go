// Package server runs an http.Handler over a real listener and drains it gracefully.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// ServerConfig is what Run needs besides the handler. Every value comes from the configuration (see
// internal/config); nothing here has a default of its own.
type ServerConfig struct {
	// Addr is the listen address, host:port. "127.0.0.1:0" picks a free port (see Ready).
	Addr string
	// ReadHeaderTimeout bounds how long a client may take to send its request headers.
	ReadHeaderTimeout time.Duration
	// ShutdownTimeout is how long in-flight requests get to finish after the context is cancelled. A value
	// of zero or less gives them no time: the drain fails at once and the connections are cut.
	ShutdownTimeout time.Duration
	// Ready, when non-nil, receives the bound address once the listener is open and before Run starts
	// serving. Run sends exactly once and gives up if its context is cancelled first, so an unread
	// unbuffered channel cannot wedge it; give the channel a buffer of 1.
	Ready chan<- net.Addr
}

// Run listens on cfg.Addr and serves h until ctx is cancelled, then drains.
//
// DRAINING. On cancellation Run (1) calls setDraining(true) FIRST, so readiness reports 503 to any probe
// that is answered from then on, (2) calls http.Server.Shutdown with cfg.ShutdownTimeout: the listener is
// closed at once (new connections are refused) and requests already in flight are allowed to finish.
// It returns nil when every request finished inside the timeout. If the timeout expires first it closes
// the remaining connections and returns an error wrapping context.DeadlineExceeded.
//
// WHAT A PROBE SEES. Shutdown closes the listener immediately, so a probe that opens a NEW connection
// during the drain is refused rather than answered 503; the 503 is what a request already in flight (or a
// keep-alive connection that has not yet gone idle) receives. A load balancer treats both as unhealthy.
// A grace period that keeps the listener open while probes observe the 503 would be a new dial, and is
// not part of Phase 0.
//
// Requests are served under their own context, not ctx: cancelling ctx must not cancel the work Run is
// waiting for. setDraining and log may be nil.
func Run(ctx context.Context, cfg ServerConfig, h http.Handler, setDraining func(bool), log *slog.Logger) error {
	if setDraining == nil {
		setDraining = func(bool) {}
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen on %q: %w", cfg.Addr, err)
	}
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		// The server's own diagnostics (TLS handshake noise, unrecovered handler panics) in the same
		// structured stream as everything else.
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	log.Info("http server listening", "addr", ln.Addr().String())

	if cfg.Ready != nil {
		select {
		case cfg.Ready <- ln.Addr():
		case <-ctx.Done():
		}
	}

	select {
	case err := <-serveErr:
		// Serve returns before any shutdown only when it failed; it never returns nil.
		_ = srv.Close()
		return fmt.Errorf("http server stopped unexpectedly: %w", err)
	case <-ctx.Done():
	}

	log.Info("http server draining", "shutdown_timeout", cfg.ShutdownTimeout.String())
	setDraining(true)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		// The drain ran out of time: cut what is left so Run does not return with live connections.
		_ = srv.Close()
		<-serveErr
		log.Error("http server drain timed out; remaining connections were closed", "shutdown_timeout", cfg.ShutdownTimeout.String())
		return fmt.Errorf("graceful shutdown did not finish within %s: %w", cfg.ShutdownTimeout, err)
	}
	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http server: %w", err)
	}
	log.Info("http server stopped", "addr", ln.Addr().String())
	return nil
}
