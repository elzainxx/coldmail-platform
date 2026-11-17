// Command fx is the platform harness's FIXTURE for module 1's real verifier node: the recipient side of the Internet, as small and as controllable as the proofs
// need. One process, standard library only, four roles that share one state (state.go):
//
//	dns    an authoritative DNS server (UDP and TCP) for the fixture zones, with per-name SERVFAIL, REFUSED, silence and NXDOMAIN on command, NXDOMAIN and NODATA with an
//	       SOA, and a forwarder for the dotless names that belong to Docker (dns.go)
//	https  the "what is my IP" endpoint the node's host audit asks for its own address (ipify.go)
//	smtp   the recipient's mail host: answers RCPT TO per (domain, local part) from rules a test can replace (smtp.go)
//	api    the control API and the readiness probe (api.go)
//
// `fx selfcheck` is the client side of the same program: run from another container on the fixture network it asks every role for what the fixture file says it
// must answer and prints one line per check (selfcheck.go). That is how the harness proves the fixtures alone, before the node or module 1 is involved.
//
// Environment (every value is checked; a bad one stops the program instead of being ignored):
//
//	FX_CONFIG         the fixture file, default /fx/fixture.json (${FX_SELF_IP} and ${FX_NODE_IP} in it are expanded from the environment)
//	FX_SELF_IP        the fixture's own address on the fixture network; FX_NODE_IP the verifier node's
//	FX_ROLES          comma list of dns, https, smtp, api; default all four
//	FX_DNS_ADDR       default :53    FX_HTTPS_ADDR default :443    FX_SMTP_ADDR default :2525    FX_API_ADDR default :8025
//	FX_TLS_CERT, FX_TLS_KEY   the certificate and key of api.ipify.org (the https role needs both; the harness mints them per run)
//	FX_DNS_UPSTREAM   where dotless names are forwarded, default 127.0.0.11:53 (Docker's embedded resolver); empty = answer them NXDOMAIN
//	FX_SMTP_HOSTNAME  the name in the SMTP banner, default mx.fixture.test
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
	"strings"
	"syscall"
	"time"
)

func envOr(name, def string) string {
	if v, ok := os.LookupEnv(name); ok {
		return strings.TrimSpace(v)
	}
	return def
}

type fixtureOpts struct {
	roles      map[string]bool
	dnsAddr    string // UDP (and TCP too, unless dnsTCPAddr says otherwise)
	dnsTCPAddr string // empty: the TCP listener takes the UDP listener's port (53 in the harness); a test passes 127.0.0.1:0 so that the kernel picks both ports independently
	httpsAddr  string
	smtpAddr   string
	apiAddr    string
	cert       *tls.Certificate
	hostname   string
	logf       func(string, ...any)
}

type fixture struct {
	st       *state
	dns      *dnsServer
	smtp     *smtpServer
	ipify    *ipifyServer
	apiSrv   *http.Server
	dnsAddr  string // UDP
	dnsTCP   string
	httpsA   string
	smtpAddr string
	apiAddr  string
}

// startFixture binds every listener of the requested roles first (a port that cannot be bound fails here, with its name), then starts serving.
func startFixture(st *state, o fixtureOpts) (*fixture, error) {
	f := &fixture{st: st}
	var closers []func()
	fail := func(err error) (*fixture, error) {
		for _, c := range closers {
			c()
		}
		return nil, err
	}
	if o.roles["dns"] {
		pc, err := net.ListenPacket("udp", o.dnsAddr)
		if err != nil {
			return fail(fmt.Errorf("cannot listen for DNS over UDP on %q: %w", o.dnsAddr, err))
		}
		closers = append(closers, func() { _ = pc.Close() })
		tcpAddr := o.dnsTCPAddr
		if tcpAddr == "" {
			host, port, _ := net.SplitHostPort(pc.LocalAddr().String())
			tcpAddr = net.JoinHostPort(host, port)
		}
		tl, err := net.Listen("tcp", tcpAddr)
		if err != nil {
			return fail(fmt.Errorf("cannot listen for DNS over TCP on %q: %w", tcpAddr, err))
		}
		closers = append(closers, func() { _ = tl.Close() })
		f.dns = newDNSServer(st, pc, tl)
		f.dnsAddr = pc.LocalAddr().String()
		f.dnsTCP = tl.Addr().String()
	}
	if o.roles["https"] {
		if o.cert == nil {
			return fail(errors.New("the https role needs FX_TLS_CERT and FX_TLS_KEY"))
		}
		ln, err := net.Listen("tcp", o.httpsAddr)
		if err != nil {
			return fail(fmt.Errorf("cannot listen for HTTPS on %q: %w (a port below 1024 needs net.ipv4.ip_unprivileged_port_start=0 for a non-root user)", o.httpsAddr, err))
		}
		closers = append(closers, func() { _ = ln.Close() })
		f.ipify = newIpifyServer(st, ln, *o.cert)
		f.httpsA = ln.Addr().String()
	}
	if o.roles["smtp"] {
		ln, err := net.Listen("tcp", o.smtpAddr)
		if err != nil {
			return fail(fmt.Errorf("cannot listen for SMTP on %q: %w", o.smtpAddr, err))
		}
		closers = append(closers, func() { _ = ln.Close() })
		f.smtp = newSMTPServer(st, ln, o.hostname, o.logf)
		f.smtpAddr = ln.Addr().String()
	}
	if o.roles["api"] {
		ln, err := net.Listen("tcp", o.apiAddr)
		if err != nil {
			return fail(fmt.Errorf("cannot listen for the control API on %q: %w", o.apiAddr, err))
		}
		closers = append(closers, func() { _ = ln.Close() })
		a := &api{st: st, ready: func() bool { return true }}
		f.apiSrv = &http.Server{Handler: a.handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second}
		f.apiAddr = ln.Addr().String()
		go func() { _ = f.apiSrv.Serve(ln) }()
	}
	if f.dns != nil {
		f.dns.serve()
	}
	if f.ipify != nil {
		f.ipify.serve()
	}
	if f.smtp != nil {
		f.smtp.serve()
	}
	return f, nil
}

func (f *fixture) close() {
	if f.apiSrv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = f.apiSrv.Shutdown(ctx)
		cancel()
	}
	if f.dns != nil {
		f.dns.close()
	}
	if f.ipify != nil {
		f.ipify.close()
	}
	if f.smtp != nil {
		f.smtp.close()
	}
}

func parseRoles(s string) (map[string]bool, error) {
	roles := map[string]bool{}
	for _, r := range strings.Split(s, ",") {
		r = strings.TrimSpace(r)
		switch r {
		case "dns", "https", "smtp", "api":
			roles[r] = true
		case "":
		default:
			return nil, fmt.Errorf("FX_ROLES names %q, which is not dns, https, smtp or api", r)
		}
	}
	if len(roles) == 0 {
		return nil, errors.New("FX_ROLES names no role")
	}
	return roles, nil
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "selfcheck" {
		os.Exit(selfcheckMain(os.Args[2:], os.Stdout))
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fx: "+err.Error())
		os.Exit(1)
	}
}

func run() error {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds | log.LUTC | log.Lmsgprefix)
	log.SetPrefix("fx: ")
	roles, err := parseRoles(envOr("FX_ROLES", "dns,https,smtp,api"))
	if err != nil {
		return err
	}
	cfg, err := loadConfig(envOr("FX_CONFIG", "/fx/fixture.json"), os.Getenv)
	if err != nil {
		return err
	}
	st := newState(cfg, envOr("FX_DNS_UPSTREAM", "127.0.0.11:53"))
	o := fixtureOpts{
		roles:     roles,
		dnsAddr:   envOr("FX_DNS_ADDR", ":53"),
		httpsAddr: envOr("FX_HTTPS_ADDR", ":443"),
		smtpAddr:  envOr("FX_SMTP_ADDR", ":2525"),
		apiAddr:   envOr("FX_API_ADDR", ":8025"),
		hostname:  envOr("FX_SMTP_HOSTNAME", "mx.fixture.test"),
		logf:      log.Printf,
	}
	certFile, keyFile := envOr("FX_TLS_CERT", ""), envOr("FX_TLS_KEY", "")
	if (certFile == "") != (keyFile == "") {
		return errors.New("FX_TLS_CERT and FX_TLS_KEY must be set together")
	}
	if certFile != "" {
		pair, lerr := tls.LoadX509KeyPair(certFile, keyFile)
		if lerr != nil {
			return fmt.Errorf("cannot load the TLS certificate %s and key %s: %v (the fixture must be able to read both: the harness runs it as the user that owns the key)", certFile, keyFile, lerr)
		}
		o.cert = &pair
	}
	f, err := startFixture(st, o)
	if err != nil {
		return err
	}
	var names []string
	for _, r := range []string{"dns", "https", "smtp", "api"} {
		if roles[r] {
			names = append(names, r)
		}
	}
	log.Printf("ready: roles=%s uid=%d dns=%q https=%q smtp=%q api=%q zones=%d dns_names=%d smtp_domains=%d dns_upstream=%q",
		strings.Join(names, ","), os.Getuid(), f.dnsAddr, f.httpsA, f.smtpAddr, f.apiAddr, len(st.zones), len(st.records), len(st.smtp), st.upstream)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	s := <-sig
	log.Printf("%v: shutting down", s)
	f.close()
	return nil
}
