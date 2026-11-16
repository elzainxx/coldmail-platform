// Command sink is the platform harness's CONTROLLABLE SMTP sink.
//
// Module 3's own sink (Mailpit) accepts everything and keeps what it received; it cannot reject, delay or throttle. The platform's proofs need a sink that
// can: one that answers a recipient with a hard 550, one that answers a temporary 451 and later relents, and one that holds the final reply of every message
// for a while so that a send is IN FLIGHT at module 3 for as long as the test wants. This is that sink: a small SMTP server (EHLO, STARTTLS, AUTH PLAIN and
// LOGIN accepting any credential, MAIL FROM, RCPT TO, DATA, RSET, NOOP, QUIT) with a control API next to it (api.go). It is stdlib only, runs on the harness's
// private network and publishes nothing.
//
// Environment (every value is checked; a bad one stops the sink instead of being ignored):
//
//	SINK_SMTP_ADDR          the SMTP listener, default ":1025"
//	SINK_HTTP_ADDR          the control API listener, default ":8025"
//	SINK_CERT, SINK_KEY     the TLS certificate and key for STARTTLS (module 3's dev sink certificate); both or neither; neither = no STARTTLS
//	SINK_HOSTNAME           the name in the banner, default "smtp-ctl"
//	SINK_REQUIRE_STARTTLS   "true" (default): AUTH and MAIL are refused until STARTTLS has been done; ignored without a certificate
//	SINK_REQUIRE_AUTH       "true" (default): MAIL is refused until AUTH has succeeded
//	SINK_REPLY_DELAY_MS     a pause before every SMTP reply, 0..1000, default 8: module 3's worker can lose a reply that arrives before it has registered to read it (see Config.ReplyDelay in smtp.go)
//	SINK_MAX_MESSAGE_BYTES  default 10485760
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "sink: "+err.Error())
		os.Exit(1)
	}
}

func envOr(name, def string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return def
}

func envBool(name string, def bool) (bool, error) {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	switch v {
	case "":
		return def, nil
	case "true", "1", "yes":
		return true, nil
	case "false", "0", "no":
		return false, nil
	}
	return false, fmt.Errorf("%s must be true or false, got %q", name, v)
}

func run() error {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds | log.LUTC)
	log.SetPrefix("sink: ")

	cfg := Config{Hostname: envOr("SINK_HOSTNAME", "smtp-ctl"), Logf: log.Printf}
	var err error
	if cfg.RequireTLS, err = envBool("SINK_REQUIRE_STARTTLS", true); err != nil {
		return err
	}
	if cfg.RequireAuth, err = envBool("SINK_REQUIRE_AUTH", true); err != nil {
		return err
	}
	replyMs, perr := strconv.Atoi(envOr("SINK_REPLY_DELAY_MS", "8"))
	if perr != nil || replyMs < 0 || replyMs > 1000 {
		return fmt.Errorf("SINK_REPLY_DELAY_MS must be a whole number of milliseconds between 0 and 1000, got %q", os.Getenv("SINK_REPLY_DELAY_MS"))
	}
	cfg.ReplyDelay = time.Duration(replyMs) * time.Millisecond
	if v := envOr("SINK_MAX_MESSAGE_BYTES", ""); v != "" {
		n, perr := strconv.Atoi(v)
		if perr != nil || n < 1024 {
			return fmt.Errorf("SINK_MAX_MESSAGE_BYTES must be a whole number of at least 1024, got %q", v)
		}
		cfg.MaxMessageBytes = n
	}
	certFile, keyFile := envOr("SINK_CERT", ""), envOr("SINK_KEY", "")
	switch {
	case certFile != "" && keyFile != "":
		pair, lerr := tls.LoadX509KeyPair(certFile, keyFile)
		if lerr != nil {
			return fmt.Errorf("cannot load the TLS certificate %s and key %s: %v (the sink must be able to read both: the harness runs it as the user that owns the key)", certFile, keyFile, lerr)
		}
		cfg.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
	case certFile != "" || keyFile != "":
		return errors.New("SINK_CERT and SINK_KEY must be set together")
	}

	st := newStore()
	srv := newServer(cfg, st)

	smtpLn, err := net.Listen("tcp", envOr("SINK_SMTP_ADDR", ":1025"))
	if err != nil {
		return fmt.Errorf("cannot listen for SMTP: %w", err)
	}
	httpLn, err := net.Listen("tcp", envOr("SINK_HTTP_ADDR", ":8025"))
	if err != nil {
		_ = smtpLn.Close()
		return fmt.Errorf("cannot listen for the control API: %w", err)
	}
	api := &http.Server{Handler: srv.APIHandler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second}

	errc := make(chan error, 2)
	go func() { errc <- srv.Serve(smtpLn) }()
	go func() {
		if herr := api.Serve(httpLn); herr != nil && !errors.Is(herr, http.ErrServerClosed) {
			errc <- herr
		}
	}()
	log.Printf("ready: SMTP on %s (STARTTLS offered: %v, required: %v, AUTH required: %v), control API on %s", smtpLn.Addr(), cfg.TLS != nil, cfg.TLS != nil && cfg.RequireTLS, cfg.RequireAuth, httpLn.Addr())

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	var result error
	select {
	case s := <-sig:
		log.Printf("%v: shutting down", s)
	case result = <-errc:
		if result == nil {
			result = errors.New("the SMTP server stopped")
		}
	}
	_ = smtpLn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = api.Shutdown(ctx)
	srv.Shutdown()
	return result
}
