package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// `fx selfcheck`: the harness's first instrument, run from a SEPARATE container on the fixture network (stage M1N-1), before the verifier node or module 1 exists. It
// reads the same fixture file the server loaded and asks every role over the network for exactly what that file says it must answer: every DNS record, every mode
// (SERVFAIL, REFUSED, silence, NXDOMAIN), the negative answers with their SOA, the dotless forwarder, the HTTPS endpoint (certificate chain checked against the run's
// CA, body equal to the caller's own address), and, for every SMTP domain, the reply to RCPT TO under every rule. One line per check; the exit status is 1 when any failed.
// A fixture that answers wrongly is found here, with the name of the check, instead of surfacing three stages later as a strange verdict.

type checkEnv struct {
	dnsAddr, dnsTCPAddr, httpsAddr, smtpAddr, apiAddr string // dnsTCPAddr empty: the TCP port is dnsAddr's
	caPEM                                             []byte
	selfIP, nodeIP                                    string
	cfg                                               *fixtureConfig
	forward                                           bool // the dotless forwarder has an upstream behind it
	dnsTimeout                                        time.Duration
}

type checkResult struct {
	Name   string
	OK     bool
	Detail string
}

func pass(name, format string, args ...any) checkResult {
	return checkResult{Name: name, OK: true, Detail: fmt.Sprintf(format, args...)}
}

func fail(name, format string, args ...any) checkResult {
	return checkResult{Name: name, OK: false, Detail: fmt.Sprintf(format, args...)}
}

func normValue(typ, v string) string {
	switch typ {
	case "MX":
		f := strings.Fields(v)
		if len(f) == 2 {
			return f[0] + " " + normName(f[1])
		}
	case "PTR":
		return normName(v)
	}
	return v
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

func modeOf(cfg *fixtureConfig, name string) string {
	best, bestLen := "", -1
	for _, m := range cfg.Modes {
		n := normName(m.Name)
		if (name == n || strings.HasSuffix(name, "."+n)) && len(n) > bestLen && m.Mode != "normal" {
			best, bestLen = m.Mode, len(n)
		}
	}
	return best
}

func runChecks(ctx context.Context, e checkEnv) []checkResult {
	var out []checkResult
	add := func(r checkResult) { out = append(out, r) }
	to := e.dnsTimeout
	if to == 0 {
		to = 2 * time.Second
	}

	// ---- DNS: every record the file lists ----
	var firstA string
	for _, r := range e.cfg.Records {
		name := normName(r.Name)
		if r.Type == "PTR" {
			if rn, ok := reverseName(name); ok {
				name = rn
			}
		}
		label := fmt.Sprintf("dns %s %s", r.Type, name)
		if m := modeOf(e.cfg, name); m != "" {
			continue // a name under a mode is checked by the mode's own check below
		}
		rep, err := dnsExchange(ctx, e.dnsAddr, name, typeByName[r.Type], to)
		if err != nil {
			add(fail(label, "no answer: %v", err))
			continue
		}
		var got, want []string
		for _, a := range rep.Answers {
			if a.Type == r.Type {
				got = append(got, normValue(r.Type, a.Value))
			}
		}
		for _, v := range r.Values {
			want = append(want, normValue(r.Type, v))
		}
		switch {
		case rep.Rcode != rcodeOK || !rep.AA || !rep.RA:
			add(fail(label, "rcode %d, aa=%t ra=%t, want 0 with AA and RA set", rep.Rcode, rep.AA, rep.RA))
		case !sameSet(got, want):
			add(fail(label, "answered %v, the file says %v", got, want))
		default:
			add(pass(label, "%v", got))
		}
		if r.Type == "A" && firstA == "" {
			firstA = name
		}
	}

	// ---- DNS: the modes ----
	for _, m := range e.cfg.Modes {
		name := normName(m.Name)
		if m.Mode == "normal" {
			continue
		}
		label := fmt.Sprintf("dns mode %s %s", m.Mode, name)
		rep, err := dnsExchange(ctx, e.dnsAddr, name, typeMX, to)
		switch m.Mode {
		case "silent":
			if err == nil {
				add(fail(label, "answered (rcode %d), must stay silent", rep.Rcode))
			} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
				add(pass(label, "no answer within %v", to))
			} else {
				add(fail(label, "unexpected error instead of silence: %v", err))
			}
		case "servfail", "refused", "nxdomain":
			want := map[string]int{"servfail": rcodeFail, "refused": rcodeRefused, "nxdomain": rcodeNX}[m.Mode]
			switch {
			case err != nil:
				add(fail(label, "no answer: %v", err))
			case rep.Rcode != want:
				add(fail(label, "rcode %d, want %d", rep.Rcode, want))
			default:
				add(pass(label, "rcode %d", rep.Rcode))
			}
		}
	}

	// ---- DNS: negative answers, TCP, the forwarder ----
	if firstA != "" {
		rep, err := dnsExchange(ctx, e.dnsAddr, firstA, typeAAAA, to)
		switch {
		case err != nil:
			add(fail("dns AAAA NODATA", "no answer: %v", err))
		case rep.Rcode != rcodeOK || len(rep.Answers) != 0 || len(rep.Authority) != 1 || !rep.AA:
			add(fail("dns AAAA NODATA", "rcode %d, %d answer(s), %d authority record(s), aa=%t: want NOERROR, none, one SOA, AA", rep.Rcode, len(rep.Answers), len(rep.Authority), rep.AA))
		default:
			add(pass("dns AAAA NODATA", "%s has no AAAA: NOERROR with an SOA", firstA))
		}
		tcpAddr := e.dnsTCPAddr
		if tcpAddr == "" {
			tcpAddr = e.dnsAddr
		}
		if rep, err := dnsExchangeTCP(ctx, tcpAddr, firstA, typeA, to); err != nil || rep.Rcode != rcodeOK || len(rep.Answers) == 0 {
			add(fail("dns over TCP", "A %s: err=%v rcode=%d answers=%d", firstA, err, rep.Rcode, len(rep.Answers)))
		} else {
			add(pass("dns over TCP", "A %s -> %v", firstA, rep.Answers[0].Value))
		}
	}
	if len(e.cfg.Zones) > 0 {
		absent := "selfcheck-absent." + e.cfg.Zones[0]
		rep, err := dnsExchange(ctx, e.dnsAddr, absent, typeMX, to)
		switch {
		case err != nil:
			add(fail("dns NXDOMAIN", "no answer: %v", err))
		case rep.Rcode != rcodeNX || len(rep.Authority) != 1:
			add(fail("dns NXDOMAIN", "%s: rcode %d with %d authority record(s), want 3 with the zone's SOA", absent, rep.Rcode, len(rep.Authority)))
		default:
			add(pass("dns NXDOMAIN", "%s: NXDOMAIN with an SOA", absent))
		}
	}
	if e.forward {
		rep, err := dnsExchange(ctx, e.dnsAddr, "postgres", typeA, 5*time.Second)
		switch {
		case err != nil:
			add(fail("dns dotless forwarder", "postgres: no answer: %v (the fixture forwards dotless names to Docker's resolver)", err))
		case rep.Rcode != rcodeOK || len(rep.Answers) == 0:
			add(fail("dns dotless forwarder", "postgres: rcode %d, %d answer(s)", rep.Rcode, len(rep.Answers)))
		default:
			add(pass("dns dotless forwarder", "postgres -> %s (resolved by Docker behind the fixture)", rep.Answers[0].Value))
		}
	}

	// ---- HTTPS: the ipify endpoint ----
	add(checkIpify(ctx, e))

	// ---- the control API ----
	if e.apiAddr != "" {
		hc := &http.Client{Timeout: 5 * time.Second}
		for _, p := range []string{"/readyz", "/api/state"} {
			resp, err := hc.Get("http://" + e.apiAddr + p)
			switch {
			case err != nil:
				add(fail("api GET "+p, "%v", err))
			case resp.StatusCode != 200:
				add(fail("api GET "+p, "HTTP %d", resp.StatusCode))
				resp.Body.Close()
			default:
				resp.Body.Close()
				add(pass("api GET "+p, "200"))
			}
		}
	}

	// ---- SMTP: every rule of every domain ----
	for _, d := range e.cfg.SMTP {
		dom := normName(d.Domain)
		for k, rule := range d.Rules {
			local, ok := sampleLocal(d.Rules, k)
			label := fmt.Sprintf("smtp %s rule %q", dom, rule.Match)
			if !ok {
				add(pass(label, "shadowed by an earlier rule: not reachable, not checked"))
				continue
			}
			addr := local + "@" + dom
			got, kind, err := smtpProbe(ctx, e.smtpAddr, addr, 1500*time.Millisecond)
			if err != nil {
				add(fail(label, "%s: %v", addr, err))
				continue
			}
			switch rule.action() {
			case "reply":
				want := strings.ReplaceAll(rule.Reply, "{addr}", addr)
				if kind != "reply" || got != want {
					add(fail(label, "%s: got %s %q, want the reply %q", addr, kind, got, want))
				} else {
					add(pass(label, "%s -> %s", addr, got))
				}
			case "stall":
				if rule.StallMs != 0 && rule.StallMs < 3000 {
					add(pass(label, "stall of %d ms is shorter than the check's patience: not checked", rule.StallMs))
				} else if kind != "stalled" {
					add(fail(label, "%s: got %s %q, want silence", addr, kind, got))
				} else {
					add(pass(label, "%s -> no reply within 1.5 s", addr))
				}
			case "drop":
				if kind != "dropped" {
					add(fail(label, "%s: got %s %q, want the connection closed with no reply", addr, kind, got))
				} else {
					add(pass(label, "%s -> connection closed with no reply", addr))
				}
			}
		}
	}
	return out
}

// sampleLocal picks a local part that rule k matches and no earlier rule does.
func sampleLocal(rules []smtpRule, k int) (string, bool) {
	cand := strings.ReplaceAll(rules[k].Match, "*", "x")
	if cand == "" || !globMatch(rules[k].Match, cand) {
		return "", false
	}
	for j := 0; j < k; j++ {
		if globMatch(rules[j].Match, cand) {
			return "", false
		}
	}
	return cand, true
}

// smtpProbe speaks the node's conversation up to RCPT TO and reports what the server did with it: the reply line, a stall (no reply within patience) or a close.
func smtpProbe(ctx context.Context, addr, rcpt string, patience time.Duration) (string, string, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return "", "", err
	}
	defer c.Close()
	br := bufio.NewReader(c)
	read := func(wait time.Duration) (string, error) {
		_ = c.SetReadDeadline(time.Now().Add(wait))
		l, err := br.ReadString('\n')
		return strings.TrimRight(l, "\r\n"), err
	}
	send := func(s string) error {
		_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
		_, err := io.WriteString(c, s+"\r\n")
		return err
	}
	for _, step := range []struct{ cmd, want string }{{"", "220"}, {"EHLO selfcheck.fixture.test", "250"}, {"MAIL FROM:<selfcheck@verifier.fixture.test>", "250"}} {
		if step.cmd != "" {
			if err := send(step.cmd); err != nil {
				return "", "", err
			}
		}
		l, err := read(5 * time.Second)
		if err != nil {
			return "", "", fmt.Errorf("before RCPT (%s): %w", step.want, err)
		}
		if !strings.HasPrefix(l, step.want) {
			return "", "", fmt.Errorf("before RCPT: got %q, want %s", l, step.want)
		}
	}
	if err := send("RCPT TO:<" + rcpt + ">"); err != nil {
		return "", "", err
	}
	l, err := read(patience)
	switch {
	case err == nil:
		return l, "reply", nil
	case errors.Is(err, io.EOF):
		return "", "dropped", nil
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		return "", "stalled", nil
	}
	return "", "", err
}

// checkIpify asks the HTTPS endpoint the way the node does: a plain GET of https://api.ipify.org through the fixture's address, the chain checked against the run's CA, and
// the body compared with the address this very connection came from.
func checkIpify(ctx context.Context, e checkEnv) checkResult {
	const name = "https api.ipify.org"
	if e.httpsAddr == "" {
		return pass(name, "no https role: not checked")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(e.caPEM) {
		return fail(name, "the CA file holds no certificate")
	}
	var local string
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "api.ipify.org", MinVersion: tls.VersionTLS12},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			c, err := d.DialContext(ctx, network, e.httpsAddr)
			if err == nil {
				local = hostOf(c.LocalAddr().String())
			}
			return c, err
		},
		DisableKeepAlives: true,
	}
	hc := &http.Client{Transport: tr, Timeout: 8 * time.Second}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.ipify.org", nil)
	resp, err := hc.Do(req)
	if err != nil {
		return fail(name, "%v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	switch {
	case resp.StatusCode != 200:
		return fail(name, "HTTP %d", resp.StatusCode)
	case string(b) != local:
		return fail(name, "the body is %q, this connection came from %q (the body must be the caller's address, no newline)", b, local)
	}
	return pass(name, "certificate chain verified against the run's CA; body %q is this caller's address, %d bytes, no newline", b, len(b))
}

// selfcheckMain is `fx selfcheck`: it builds the checks from the environment, prints one line each and a summary, and returns the exit status.
func selfcheckMain(args []string, w io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintln(w, "usage: fx selfcheck   (configured by FX_CONFIG, FX_SELF_IP, FX_NODE_IP, FX_CHECK_CA and, to override addresses, FX_CHECK_DNS/HTTPS/SMTP/API)")
		return 2
	}
	self := envOr("FX_SELF_IP", "")
	if self == "" {
		fmt.Fprintln(w, "fx selfcheck: FX_SELF_IP is not set")
		return 2
	}
	cfg, err := loadConfig(envOr("FX_CONFIG", "/fx/fixture.json"), os.Getenv)
	if err != nil {
		fmt.Fprintln(w, "fx selfcheck: "+err.Error())
		return 2
	}
	ca, err := os.ReadFile(envOr("FX_CHECK_CA", "/fx/ca.pem"))
	if err != nil {
		fmt.Fprintln(w, "fx selfcheck: cannot read the CA: "+err.Error())
		return 2
	}
	e := checkEnv{
		dnsAddr:   envOr("FX_CHECK_DNS", net.JoinHostPort(self, "53")),
		httpsAddr: envOr("FX_CHECK_HTTPS", net.JoinHostPort(self, "443")),
		smtpAddr:  envOr("FX_CHECK_SMTP", net.JoinHostPort(self, "2525")),
		apiAddr:   envOr("FX_CHECK_API", net.JoinHostPort(self, "8025")),
		caPEM:     ca, selfIP: self, nodeIP: envOr("FX_NODE_IP", ""), cfg: cfg,
		forward: envOr("FX_CHECK_FORWARD", "") == "1",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	res := runChecks(ctx, e)
	return printChecks(w, res)
}

func printChecks(w io.Writer, res []checkResult) int {
	bad := 0
	for _, r := range res {
		tag := "ok  "
		if !r.OK {
			tag = "FAIL"
			bad++
		}
		fmt.Fprintf(w, "  [%s] %s: %s\n", tag, r.Name, r.Detail)
	}
	fmt.Fprintf(w, "fx selfcheck: %d check(s), %d failed\n", len(res), bad)
	if len(res) == 0 {
		fmt.Fprintln(w, "fx selfcheck: RED: no check ran (the instrument matched nothing)")
		return 1
	}
	if bad > 0 {
		return 1
	}
	return 0
}
