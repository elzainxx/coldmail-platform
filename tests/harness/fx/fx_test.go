package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests run in the `go` workflow (no build tag, no database, no Docker). They start the real fixture on loopback sockets and talk to it the way the node and module 1
// do: Go's own DNS client for the control plane's side, raw packets for the exact answers, a hand-rolled SMTP client for the node's conversation, a TLS client that verifies the
// chain for the node's "what is my IP" call. Each test that proves a behaviour BITES has its paired control: the same request with the knob off, which must behave the other way.

const (
	testSelf = "127.0.0.1"
	testNode = "127.0.0.2"
)

func testEnv(name string) string {
	switch name {
	case "FX_SELF_IP":
		return testSelf
	case "FX_NODE_IP":
		return testNode
	}
	return ""
}

func shippedConfig(t *testing.T) *fixtureConfig {
	t.Helper()
	cfg, err := loadConfig("../m1node/fixture.json", testEnv)
	if err != nil {
		t.Fatalf("the shipped fixture file does not load: %v", err)
	}
	return cfg
}

type testCA struct {
	pem  []byte
	cert tls.Certificate
}

func newTestCA(t *testing.T, host string) testCA {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fx test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, _ := x509.ParseCertificate(caDER)
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: host}, DNSNames: []string{host},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return testCA{
		pem:  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		cert: tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: leafKey},
	}
}

type testFx struct {
	t    *testing.T
	st   *state
	cfg  *fixtureConfig
	f    *fixture
	ca   testCA
	logs *bytes.Buffer
}

// startTestFx starts the whole fixture on loopback. upstream is where dotless names go ("" = nowhere).
func startTestFx(t *testing.T, upstream string) *testFx {
	t.Helper()
	cfg := shippedConfig(t)
	st := newState(cfg, upstream)
	ca := newTestCA(t, "api.ipify.org")
	logs := &bytes.Buffer{}
	var logMu sync.Mutex // the DNS, SMTP and HTTPS goroutines all log: a bytes.Buffer is not safe for that (-race on the runner)
	// Every listener binds port 0 on loopback and the test reads the kernel's choice back (f.dnsAddr, f.dnsTCP, f.httpsA, f.smtpAddr, f.apiAddr): no fixed or reused port, so the tests pass
	// on a host that refuses some ports (Windows excludes ranges) and on Linux alike. The UDP and the TCP side of DNS are chosen independently (dnsTCPAddr).
	f, err := startFixture(st, fixtureOpts{
		roles:   map[string]bool{"dns": true, "https": true, "smtp": true, "api": true},
		dnsAddr: testSelf + ":0", dnsTCPAddr: testSelf + ":0", httpsAddr: testSelf + ":0", smtpAddr: testSelf + ":0", apiAddr: testSelf + ":0",
		cert: &ca.cert, hostname: "mx.fixture.test", logf: func(format string, args ...any) {
			logMu.Lock()
			defer logMu.Unlock()
			fmt.Fprintf(logs, format+"\n", args...)
		},
	})
	if err != nil {
		t.Fatalf("cannot start the fixture: %v", err)
	}
	t.Cleanup(f.close)
	return &testFx{t: t, st: st, cfg: cfg, f: f, ca: ca, logs: logs}
}

func (x *testFx) ctx() context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	x.t.Cleanup(cancel)
	return ctx
}

func (x *testFx) dns(name string, qtype uint16) dnsReply {
	x.t.Helper()
	r, err := dnsExchange(x.ctx(), x.f.dnsAddr, name, qtype, 2*time.Second)
	if err != nil {
		x.t.Fatalf("DNS %s %s: %v", typeName(qtype), name, err)
	}
	return r
}

func (x *testFx) put(path string, body any) (int, string) {
	x.t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPut, "http://"+x.f.apiAddr+path, bytes.NewReader(b))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		x.t.Fatalf("PUT %s: %v", path, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

func (x *testFx) get(path string, into any) int {
	x.t.Helper()
	resp, err := http.Get("http://" + x.f.apiAddr + path)
	if err != nil {
		x.t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	if into != nil {
		if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
			x.t.Fatalf("GET %s: %v", path, err)
		}
	}
	return resp.StatusCode
}

func (x *testFx) rcpt(addr string) (string, string) {
	x.t.Helper()
	got, kind, err := smtpProbe(x.ctx(), x.f.smtpAddr, addr, 800*time.Millisecond)
	if err != nil {
		x.t.Fatalf("SMTP RCPT %s: %v", addr, err)
	}
	return got, kind
}

func answers(r dnsReply, typ string) []string {
	var out []string
	for _, a := range r.Answers {
		if a.Type == typ {
			out = append(out, a.Value)
		}
	}
	return out
}

// ---- the file -------------------------------------------------------------------------------------------------------------------------------------------

// TestConfigLoadsAndIsConsistent: the shipped fixture file loads, every SMTP domain has a mail route in DNS that leads to the SMTP host (or, for the one recipe that must
// find nothing listening, to the node's address), and the loader refuses the mistakes that would be silent otherwise (the control: each is its own error).
func TestConfigLoadsAndIsConsistent(t *testing.T) {
	cfg := shippedConfig(t)
	if len(cfg.SMTP) < 15 || len(cfg.Records) < 30 || len(cfg.Modes) != 3 {
		t.Fatalf("the shipped file holds %d smtp domains, %d records, %d modes: the recipe table is not all there", len(cfg.SMTP), len(cfg.Records), len(cfg.Modes))
	}
	st := newState(cfg, "")
	for _, d := range cfg.SMTP {
		dom := normName(d.Domain)
		mx := st.records[dom]["MX"]
		if len(mx) != 1 {
			t.Fatalf("the SMTP domain %s has %d MX record(s), want one", dom, len(mx))
		}
		_, host, err := parseMX(mx[0])
		if err != nil {
			t.Fatal(err)
		}
		a := st.records[normName(host)]["A"]
		if len(a) != 1 || a[0] != testSelf {
			t.Fatalf("the MX host %s of %s answers %v, want the fixture's own address %s", host, dom, a, testSelf)
		}
	}
	// the control: the loader's refusals
	good, _ := os.ReadFile("../m1node/fixture.json")
	cases := []struct{ name, text, want string }{
		{"an unset placeholder", string(good), "${FX_NODE_IP} but the environment variable is not set"},
		{"another placeholder", strings.Replace(string(good), "${FX_SELF_IP}", "${HOME}", 1), "not one of FX_SELF_IP, FX_NODE_IP"},
		{"an unknown key", strings.Replace(string(good), `"ttl": 1`, `"ttl": 1, "tll": 2`, 1), "unknown field"},
		{"a bad rule", strings.Replace(string(good), `"match":"nx-*","reply":"550 5.1.1 <{addr}> User unknown"`, `"match":"nx-*","reply":"banana"`, 1), "not an SMTP reply line"},
		{"a bad mode", strings.Replace(string(good), `"mode":"servfail"`, `"mode":"explode"`, 1), "one of normal, servfail"},
	}
	for _, c := range cases {
		env := testEnv
		if c.name == "an unset placeholder" {
			env = func(n string) string {
				if n == "FX_SELF_IP" {
					return testSelf
				}
				return ""
			}
		}
		_, err := parseConfig([]byte(c.text), env)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: got %v, want an error containing %q", c.name, err, c.want)
		}
	}
	if _, err := parseConfig(good, testEnv); err != nil {
		t.Fatalf("the control (the unmodified file) must load: %v", err)
	}
}

// ---- DNS ------------------------------------------------------------------------------------------------------------------------------------------------

// TestDNSAnswersAndNegatives: positive answers carry AA and RA, a missing name is NXDOMAIN and an existing name without the type is NODATA, both with the zone's SOA (a
// resolver that gets neither would retry or stall), AAAA is NODATA, the null MX is MX 0 ".", and the reverse zone answers the node's PTR.
func TestDNSAnswersAndNegatives(t *testing.T) {
	x := startTestFx(t, "")
	mx := x.dns("valid.fixture.test", typeMX)
	if mx.Rcode != rcodeOK || !mx.AA || !mx.RA || strings.Join(answers(mx, "MX"), ",") != "10 mx.valid.fixture.test" {
		t.Fatalf("MX valid.fixture.test: %+v", mx)
	}
	if a := x.dns("mx.valid.fixture.test", typeA); strings.Join(answers(a, "A"), ",") != testSelf {
		t.Fatalf("A mx.valid.fixture.test: %+v", a)
	}
	rev, _ := reverseName(testNode) // the node's address as its in-addr.arpa name
	if p := x.dns(rev, typePTR); strings.Join(answers(p, "PTR"), ",") != "verifier.fixture.test" {
		t.Fatalf("PTR of the node's address: %+v", p)
	}
	if v := x.dns("verifier.fixture.test", typeA); strings.Join(answers(v, "A"), ",") != testNode {
		t.Fatalf("A verifier.fixture.test: %+v", v)
	}
	if n := x.dns("nullmx.fixture.test", typeMX); strings.Join(answers(n, "MX"), ",") != "0 " {
		t.Fatalf("the null MX: %+v (want preference 0 and the root as the exchange)", n)
	}
	// NODATA: the name exists, the type does not
	nd := x.dns("mx.valid.fixture.test", typeAAAA)
	if nd.Rcode != rcodeOK || len(nd.Answers) != 0 || len(nd.Authority) != 1 || nd.Authority[0].Type != "SOA" || !nd.AA {
		t.Fatalf("AAAA of a name that has only an A record must be NOERROR, no answer, one SOA, AA set: %+v", nd)
	}
	nm := x.dns("nodata.fixture.test", typeMX)
	if nm.Rcode != rcodeOK || len(nm.Answers) != 0 || len(nm.Authority) != 1 {
		t.Fatalf("MX of the TXT-only name must be NODATA with an SOA: %+v", nm)
	}
	if tx := x.dns("nodata.fixture.test", typeTXT); len(answers(tx, "TXT")) != 1 {
		t.Fatalf("the TXT-only name has no TXT: %+v", tx)
	}
	// NXDOMAIN
	nx := x.dns("nomx.fixture.test", typeMX)
	if nx.Rcode != rcodeNX || len(nx.Authority) != 1 || nx.Authority[0].Type != "SOA" || !nx.AA {
		t.Fatalf("a name that does not exist must be NXDOMAIN with the zone's SOA: %+v", nx)
	}
	// an empty non-terminal is NODATA, not NXDOMAIN (fixture.test has names below it but none of its own)
	if e := x.dns("fixture.test", typeA); e.Rcode != rcodeOK || len(e.Answers) != 0 {
		t.Fatalf("fixture.test exists only as the parent of other names: want NODATA, got %+v", e)
	}
	// outside every zone and dotted: NXDOMAIN too (the control plane must never see a name we did not write)
	if o := x.dns("example.org", typeA); o.Rcode != rcodeNX {
		t.Fatalf("a dotted name outside the zones must be NXDOMAIN: %+v", o)
	}
	// the control of the whole test: a type the name has is answered, a type it lacks is not
	if len(answers(x.dns("mx.valid.fixture.test", typeA), "A")) != 1 || len(answers(x.dns("mx.valid.fixture.test", typeMX), "MX")) != 0 {
		t.Fatal("the control: the A answers and the MX of the same name does not")
	}
}

// TestDNSModes: SERVFAIL, REFUSED and NXDOMAIN answer with their rcode, SILENT answers nothing, a mode covers the names below it, and the control API flips ONE name and
// back without touching its neighbour.
func TestDNSModes(t *testing.T) {
	x := startTestFx(t, "")
	for name, want := range map[string]int{"servfail.fixture.test": rcodeFail, "mx.servfail.fixture.test": rcodeFail, "refused.fixture.test": rcodeRefused} {
		if r := x.dns(name, typeMX); r.Rcode != want {
			t.Fatalf("%s: rcode %d, want %d", name, r.Rcode, want)
		}
	}
	if _, err := dnsExchange(x.ctx(), x.f.dnsAddr, "silent.fixture.test", typeMX, 500*time.Millisecond); err == nil {
		t.Fatal("the silent name answered")
	} else if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
		t.Fatalf("the silent name must produce a timeout, got %v", err)
	}
	// flip one name through the API: the neighbour is untouched (the control), and normal puts it back
	if r := x.dns("ctl-dns.fixture.test", typeMX); len(answers(r, "MX")) != 1 {
		t.Fatalf("before the flip ctl-dns answers normally: %+v", r)
	}
	for _, mode := range []struct {
		m    string
		want int
	}{{"servfail", rcodeFail}, {"refused", rcodeRefused}, {"nxdomain", rcodeNX}} {
		if code, body := x.put("/api/dns/modes", map[string]string{"name": "ctl-dns.fixture.test", "mode": mode.m}); code != 200 {
			t.Fatalf("PUT mode %s: %d %s", mode.m, code, body)
		}
		if r := x.dns("ctl-dns.fixture.test", typeMX); r.Rcode != mode.want {
			t.Fatalf("ctl-dns in mode %s: rcode %d, want %d", mode.m, r.Rcode, mode.want)
		}
		if r := x.dns("valid.fixture.test", typeMX); r.Rcode != rcodeOK || len(answers(r, "MX")) != 1 {
			t.Fatalf("flipping ctl-dns to %s changed its neighbour valid.fixture.test: %+v", mode.m, r)
		}
	}
	x.put("/api/dns/modes", map[string]string{"name": "ctl-dns.fixture.test", "mode": "silent"})
	if _, err := dnsExchange(x.ctx(), x.f.dnsAddr, "ctl-dns.fixture.test", typeMX, 400*time.Millisecond); err == nil {
		t.Fatal("ctl-dns in mode silent answered")
	}
	x.put("/api/dns/modes", map[string]string{"name": "ctl-dns.fixture.test", "mode": "normal"})
	if r := x.dns("ctl-dns.fixture.test", typeMX); r.Rcode != rcodeOK || len(answers(r, "MX")) != 1 {
		t.Fatalf("after mode normal ctl-dns must answer again: %+v", r)
	}
	// a record changed through the API: the A of an MX host flips to the node's address and back
	x.put("/api/dns/records", map[string]any{"name": "mx.ctl-conn.fixture.test", "type": "A", "values": []string{testNode}})
	if a := x.dns("mx.ctl-conn.fixture.test", typeA); strings.Join(answers(a, "A"), ",") != testNode {
		t.Fatalf("the replaced A record: %+v", a)
	}
	x.put("/api/dns/records", map[string]any{"name": "mx.ctl-conn.fixture.test", "type": "A", "values": []string{testSelf}})
	if a := x.dns("mx.ctl-conn.fixture.test", typeA); strings.Join(answers(a, "A"), ",") != testSelf {
		t.Fatalf("the restored A record: %+v", a)
	}
	// reset puts every table back and forgets the logs
	x.put("/api/dns/modes", map[string]string{"name": "valid.fixture.test", "mode": "refused"})
	resp, err := http.Post("http://"+x.f.apiAddr+"/api/reset", "application/json", nil)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("reset: %v %v", err, resp)
	}
	resp.Body.Close()
	if r := x.dns("valid.fixture.test", typeMX); r.Rcode != rcodeOK {
		t.Fatalf("reset must undo the mode: %+v", r)
	}
	if n := x.st.counts().DNSQueries; n != 1 {
		t.Fatalf("reset must forget the query log: %d queries counted after it, want the one just made", n)
	}
}

// TestDNSWithGoResolver: what module 1's control plane sees, with Go's own resolver (the same code path as its layers): MX and address lookups, and the three failure
// classes it tells apart (not found, temporary "server misbehaving" = SERVFAIL, non-temporary = REFUSED, timeout = silence).
// Every name is rooted (a trailing dot): the host's resolv.conf may carry search domains, and Go then retries a failed name with each suffix, which turned the runner's SERVFAIL into "no such host".
func TestDNSWithGoResolver(t *testing.T) {
	x := startTestFx(t, "")
	r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "udp", x.f.dnsAddr)
	}}
	ctx := x.ctx()
	mxs, err := r.LookupMX(ctx, "valid.fixture.test.")
	if err != nil || len(mxs) != 1 || mxs[0].Pref != 10 || mxs[0].Host != "mx.valid.fixture.test." {
		t.Fatalf("LookupMX valid: %v %v", mxs, err)
	}
	ips, err := r.LookupIPAddr(ctx, "mx.valid.fixture.test.")
	if err != nil || len(ips) != 1 || ips[0].IP.String() != testSelf {
		t.Fatalf("LookupIPAddr (A and AAAA, as the control plane asks): %v %v", ips, err)
	}
	if names, err := r.LookupAddr(ctx, testNode); err != nil || len(names) != 1 || names[0] != "verifier.fixture.test." {
		t.Fatalf("LookupAddr of the node: %v %v", names, err)
	}
	if mxs, err := r.LookupMX(ctx, "nullmx.fixture.test."); err != nil || len(mxs) != 1 || mxs[0].Pref != 0 || strings.Trim(mxs[0].Host, ".") != "" {
		t.Fatalf("the null MX through Go's resolver: %v %v (module 1 recognises MX 0 \".\")", mxs, err)
	}
	for _, name := range []string{"nomx.fixture.test", "nodata.fixture.test"} {
		_, err := r.LookupMX(ctx, name+".")
		var de *net.DNSError
		if !errors.As(err, &de) || !de.IsNotFound {
			t.Fatalf("LookupMX %s: %v, want a not-found DNSError (NXDOMAIN and NODATA both)", name, err)
		}
		if _, err := r.LookupIPAddr(ctx, name+"."); !errors.As(err, &de) || !de.IsNotFound {
			t.Fatalf("LookupIPAddr %s: %v, want not found (implicit MX finds no address)", name, err)
		}
	}
	var de *net.DNSError
	_, err = r.LookupMX(ctx, "servfail.fixture.test.")
	if !errors.As(err, &de) || !de.IsTemporary || de.Err != "server misbehaving" || de.IsNotFound || de.IsTimeout {
		t.Fatalf("SERVFAIL must reach the resolver's caller as a temporary \"server misbehaving\": %#v", err)
	}
	_, err = r.LookupMX(ctx, "refused.fixture.test.")
	if !errors.As(err, &de) || de.IsTemporary || de.IsNotFound || de.IsTimeout {
		t.Fatalf("REFUSED must be neither temporary, not-found nor a timeout (module 1 calls it undetermined): %#v", err)
	}
	short, cancel := context.WithTimeout(ctx, 700*time.Millisecond)
	defer cancel()
	_, err = r.LookupMX(short, "silent.fixture.test.")
	if !errors.As(err, &de) || !de.IsTimeout {
		t.Fatalf("silence must be a timeout: %#v", err)
	}
}

// TestDNSOverTCPAndMalformedPackets: the same answers over TCP, and the packets that are not queries are ignored or refused without a crash.
func TestDNSOverTCPAndMalformedPackets(t *testing.T) {
	x := startTestFx(t, "")
	r, err := dnsExchangeTCP(x.ctx(), x.f.dnsTCP, "valid.fixture.test", typeMX, 2*time.Second)
	if err != nil || r.Rcode != rcodeOK || len(answers(r, "MX")) != 1 {
		t.Fatalf("MX over TCP: %v %+v", err, r)
	}
	c, err := net.Dial("udp", x.f.dnsAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	send := func(pkt []byte) []byte {
		_ = c.SetDeadline(time.Now().Add(400 * time.Millisecond))
		if _, err := c.Write(pkt); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 600)
		n, err := c.Read(buf)
		if err != nil {
			return nil
		}
		return buf[:n]
	}
	if got := send([]byte{1, 2, 3}); got != nil {
		t.Fatalf("a 3-byte packet was answered: %v", got)
	}
	resp := buildQuery(7, "valid.fixture.test", typeMX)
	resp[2] |= 0x80 // QR set: already a response
	if got := send(resp); got != nil {
		t.Fatalf("a response packet was answered: %v", got)
	}
	notify := buildQuery(8, "valid.fixture.test", typeMX)
	notify[2] |= 4 << 3 // opcode 4 (NOTIFY)
	if got := send(notify); len(got) < 12 || got[3]&0xF != rcodeNotImpl {
		t.Fatalf("a non-query opcode must be answered NOTIMP: %v", got)
	}
	two := buildQuery(9, "valid.fixture.test", typeMX)
	two[5] = 2 // QDCOUNT 2
	if got := send(two); len(got) < 12 || got[3]&0xF != rcodeForm {
		t.Fatalf("two questions must be answered FORMERR: %v", got)
	}
	// the control: the well-formed query is answered on the same socket
	if got := send(buildQuery(10, "valid.fixture.test", typeMX)); len(got) < 12 || got[3]&0xF != rcodeOK {
		t.Fatalf("the well-formed query was not answered: %v", got)
	}
}

// TestDNSForwardsDotlessNames: a name with no dot is forwarded unchanged to the upstream resolver (Docker's, in the harness) and its answer comes back unchanged; with no
// upstream, or an upstream that does not answer, it is NXDOMAIN or SERVFAIL, never silence; a dotted name outside the zones is never forwarded (the control).
func TestDNSForwardsDotlessNames(t *testing.T) {
	up, err := net.ListenPacket("udp", testSelf+":0")
	if err != nil {
		t.Fatal(err)
	}
	defer up.Close()
	helper := newState(shippedConfig(t), "")
	var mu sync.Mutex
	var asked []string
	go func() {
		buf := make([]byte, 1500)
		for {
			n, addr, err := up.ReadFrom(buf)
			if err != nil {
				return
			}
			q, _, ok := parseQuery(buf[:n])
			if !ok {
				continue
			}
			mu.Lock()
			asked = append(asked, q.name)
			mu.Unlock()
			rd, _ := rdataA("10.20.30.40")
			_, _ = up.WriteTo(helper.build(q, dnsResult{rcode: rcodeOK, aa: true, answers: []rr{{typ: typeA, rdata: rd}}}), addr)
		}
	}()
	x := startTestFx(t, up.LocalAddr().String())
	if r := x.dns("postgres", typeA); r.Rcode != rcodeOK || strings.Join(answers(r, "A"), ",") != "10.20.30.40" {
		t.Fatalf("the dotless name must be forwarded and answered by the upstream: %+v", r)
	}
	if r := x.dns("example.org", typeA); r.Rcode != rcodeNX {
		t.Fatalf("a dotted name outside the zones must not be forwarded: %+v", r)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, n := range asked {
		if n != "postgres" {
			t.Fatalf("the upstream was asked for %q: only dotless names are forwarded", n)
		}
	}
	// no upstream: NXDOMAIN, not silence
	y := startTestFx(t, "")
	if r := y.dns("postgres", typeA); r.Rcode != rcodeNX {
		t.Fatalf("with no upstream a dotless name is NXDOMAIN: %+v", r)
	}
	// an upstream that never answers: SERVFAIL after the forward deadline
	dead, _ := net.ListenPacket("udp", testSelf+":0")
	defer dead.Close()
	z := startTestFx(t, dead.LocalAddr().String())
	start := time.Now()
	if r := z.dns("postgres", typeA); r.Rcode != rcodeFail {
		t.Fatalf("an upstream that does not answer must produce SERVFAIL: %+v", r)
	}
	if d := time.Since(start); d > 4*time.Second {
		t.Fatalf("the SERVFAIL took %v", d)
	}
}

// ---- SMTP -----------------------------------------------------------------------------------------------------------------------------------------------

// TestSMTPRepliesPerDomainAndLocalPart: every recipe of the table, at RCPT TO, exactly as the node will see it.
func TestSMTPRepliesPerDomainAndLocalPart(t *testing.T) {
	x := startTestFx(t, "")
	want := []struct{ addr, reply string }{
		{"ada@valid.fixture.test", "250 2.1.5 OK"},
		{"nx-0123456789abcdef01234567@valid.fixture.test", "550 5.1.1 <nx-0123456789abcdef01234567@valid.fixture.test> User unknown"},
		{"bob@full.fixture.test", "552 5.2.2 Mailbox full"},
		{"nx-aa@full.fixture.test", "550 5.1.1 <nx-aa@full.fixture.test> User unknown"},
		{"eli@gone.fixture.test", "550 5.1.1 <eli@gone.fixture.test> User unknown"},
		{"gus@catchall.fixture.test", "250 2.1.5 OK"},
		{"nx-aa@catchall.fixture.test", "250 2.1.5 OK"},
		{"kim@grey.fixture.test", "450 4.2.0 Greylisted, please try again later"},
		{"sam@blocked.fixture.test", "554 5.7.1 Client host blocked by policy"},
		{"pam@ambiguous.fixture.test", "550 5.7.0 Message rejected"},
		{"ADA@VALID.FIXTURE.TEST", "250 2.1.5 OK"}, // the match and the domain are case-insensitive
		{"ada@unknown.fixture.test", "550 5.1.2 <ada@unknown.fixture.test> Domain not served by this fixture"},
	}
	for _, w := range want {
		if got, kind := x.rcpt(w.addr); kind != "reply" || got != w.reply {
			t.Fatalf("RCPT %s: %s %q, want %q", w.addr, kind, got, w.reply)
		}
	}
	// the log has one entry per RCPT, with the rule that decided
	var entries []smtpEntry
	if x.get("/api/smtp/sessions?domain=valid.fixture.test", &entries) != 200 || len(entries) != 3 {
		t.Fatalf("the session log of valid.fixture.test: %d entries, want 3 (ada, the nx- probe, the upper-case spelling)", len(entries))
	}
	if entries[0].Rule != "*" || entries[1].Rule != "nx-*" || entries[0].Remote != testSelf || entries[0].Rcpt != "ada@valid.fixture.test" || entries[0].Helo != "selfcheck.fixture.test" {
		t.Fatalf("the first two entries: %+v", entries[:2])
	}
	// since= returns only newer entries
	var newer []smtpEntry
	x.get(fmt.Sprintf("/api/smtp/sessions?since=%d", entries[1].Seq), &newer)
	if len(newer) == 0 || newer[0].Seq <= entries[1].Seq {
		t.Fatalf("since=%d returned %+v", entries[1].Seq, newer)
	}
}

// TestSMTPStallDropAndPortChecks: a stalled RCPT gets no reply within the client's patience, a dropped RCPT is a closed connection with no reply, and a connection that opens
// and closes with no bytes (the gem's port check) is counted and harmless. The controls: the same domain's other recipient (the nx- probe) is answered normally.
func TestSMTPStallDropAndPortChecks(t *testing.T) {
	x := startTestFx(t, "")
	if got, kind := x.rcpt("oli@stall.fixture.test"); kind != "stalled" {
		t.Fatalf("the stalled recipient: %s %q, want silence", kind, got)
	}
	if got, kind := x.rcpt("nx-aa@stall.fixture.test"); kind != "reply" || !strings.HasPrefix(got, "550 5.1.1") {
		t.Fatalf("the probe of the stalling domain must be answered: %s %q", kind, got)
	}
	if got, kind := x.rcpt("quin@dropped.fixture.test"); kind != "dropped" {
		t.Fatalf("the dropped recipient: %s %q, want a closed connection", kind, got)
	}
	if got, kind := x.rcpt("nx-aa@dropped.fixture.test"); kind != "reply" || !strings.HasPrefix(got, "550 5.1.1") {
		t.Fatalf("the probe of the dropping domain must be answered: %s %q", kind, got)
	}
	before := x.st.counts().PortChecks
	for i := 0; i < 3; i++ {
		c, err := net.Dial("tcp", x.f.smtpAddr)
		if err != nil {
			t.Fatal(err)
		}
		c.Close()
	}
	deadline := time.Now().Add(2 * time.Second)
	for x.st.counts().PortChecks < before+3 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := x.st.counts().PortChecks - before; n != 3 {
		t.Fatalf("three connections without a command were counted as %d port checks", n)
	}
	var entries []smtpEntry
	x.get("/api/smtp/sessions?domain=stall.fixture.test", &entries)
	if len(entries) != 2 || entries[0].Action != "stall" || entries[1].Action != "reply" {
		t.Fatalf("the stall domain's log: %+v", entries)
	}
	var dropped []smtpEntry
	x.get("/api/smtp/sessions?domain=dropped.fixture.test", &dropped)
	if len(dropped) != 2 || dropped[0].Action != "drop" {
		t.Fatalf("the drop domain's log: %+v", dropped)
	}
}

// TestSMTPControlSwitchesOneDomain: replacing the rules of ONE domain changes its answers and nobody else's; reset restores the file; an invalid rule set is refused and
// changes nothing.
func TestSMTPControlSwitchesOneDomain(t *testing.T) {
	x := startTestFx(t, "")
	if got, _ := x.rcpt("nx-aa@ctl-catch.fixture.test"); !strings.HasPrefix(got, "550") {
		t.Fatalf("the baseline probe of ctl-catch: %q", got)
	}
	code, body := x.put("/api/smtp/domains", map[string]any{"domain": "ctl-catch.fixture.test", "rules": []map[string]string{{"match": "*", "reply": "250 2.1.5 OK"}}})
	if code != 200 {
		t.Fatalf("PUT rules: %d %s", code, body)
	}
	if got, _ := x.rcpt("nx-aa@ctl-catch.fixture.test"); !strings.HasPrefix(got, "250") {
		t.Fatalf("after the switch the probe of ctl-catch must be accepted (a catch-all): %q", got)
	}
	if got, _ := x.rcpt("nx-aa@ctl-rcpt.fixture.test"); !strings.HasPrefix(got, "550") {
		t.Fatalf("switching ctl-catch changed its neighbour ctl-rcpt: %q", got)
	}
	bad := []struct {
		name string
		body any
		want string
	}{
		{"a reply that is not a reply line", map[string]any{"domain": "ctl-catch.fixture.test", "rules": []map[string]string{{"match": "*", "reply": "OK"}}}, "not an SMTP reply line"},
		{"an unknown action", map[string]any{"domain": "ctl-catch.fixture.test", "rules": []map[string]string{{"match": "*", "action": "explode"}}}, "not reply, stall or drop"},
		{"no rules", map[string]any{"domain": "ctl-catch.fixture.test", "rules": []map[string]string{}}, "at least one rule"},
		{"an unknown field", map[string]any{"domain": "ctl-catch.fixture.test", "rules": []map[string]string{{"match": "*", "reply": "250 OK"}}, "extra": 1}, "unknown field"},
	}
	for _, b := range bad {
		if code, body := x.put("/api/smtp/domains", b.body); code != 400 || !strings.Contains(body, b.want) {
			t.Fatalf("%s: %d %s, want 400 containing %q", b.name, code, body, b.want)
		}
	}
	if got, _ := x.rcpt("nx-aa@ctl-catch.fixture.test"); !strings.HasPrefix(got, "250") {
		t.Fatalf("a refused rule set changed the rules: %q", got)
	}
	resp, err := http.Post("http://"+x.f.apiAddr+"/api/reset", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got, _ := x.rcpt("nx-aa@ctl-catch.fixture.test"); !strings.HasPrefix(got, "550") {
		t.Fatalf("reset must restore the file's rules: %q", got)
	}
}

// TestSMTPProtocolEdges: what a real client can send besides the happy path.
func TestSMTPProtocolEdges(t *testing.T) {
	x := startTestFx(t, "")
	c, err := net.Dial("tcp", x.f.smtpAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	br := bufio.NewReader(c)
	say := func(cmd string) string {
		_ = c.SetDeadline(time.Now().Add(2 * time.Second))
		if cmd != "" {
			fmt.Fprintf(c, "%s\r\n", cmd)
		}
		l, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("after %q: %v", cmd, err)
		}
		return strings.TrimRight(l, "\r\n")
	}
	steps := []struct{ cmd, prefix string }{
		{"", "220 mx.fixture.test"},
		{"RCPT TO:<ada@valid.fixture.test>", "503"}, // before MAIL
		{"EHLO node.test", "250 mx.fixture.test"},
		{"MAIL FROM:<verifier@verifier.fixture.test>", "250"},
		{"RCPT TO:ada@valid.fixture.test", "501"},   // no angle brackets
		{"RCPT TO:<nodomain>", "501"},               // no @
		{"STARTTLS", "502"},                         // never offered
		{"BOGUS", "500"},                            // unknown command
		{"RCPT TO:<ada@valid.fixture.test>", "250"}, // the happy path still works after the errors
		{"RSET", "250"},
		{"RCPT TO:<ada@valid.fixture.test>", "503"}, // RSET forgot MAIL
		{"NOOP", "250"},
		{"QUIT", "221"},
	}
	for _, s := range steps {
		if got := say(s.cmd); !strings.HasPrefix(got, s.prefix) {
			t.Fatalf("after %q: %q, want a line starting %q", s.cmd, got, s.prefix)
		}
	}
	if _, err := br.ReadString('\n'); err == nil {
		t.Fatal("the server kept the connection open after QUIT")
	}
}

// ---- the "what is my IP" endpoint ---------------------------------------------------------------------------------------------------------------------

// TestIpifyAnswersTheCallersAddress: the body is the caller's address with no newline; the chain verifies against the run's CA for api.ipify.org and not for another name
// or another CA (the controls); a request is logged.
func TestIpifyAnswersTheCallersAddress(t *testing.T) {
	x := startTestFx(t, "")
	res := checkIpify(x.ctx(), checkEnv{httpsAddr: x.f.httpsA, caPEM: x.ca.pem})
	if !res.OK {
		t.Fatalf("the ipify check: %+v", res)
	}
	var log []ipifyEntry
	if x.get("/api/ipify", &log) != 200 || len(log) != 1 || log[0].Answered != testSelf || log[0].Remote != testSelf {
		t.Fatalf("the ipify log: %+v", log)
	}
	// control 1: another CA does not verify (the node would fail its audit's IP step)
	other := newTestCA(t, "api.ipify.org")
	if res := checkIpify(x.ctx(), checkEnv{httpsAddr: x.f.httpsA, caPEM: other.pem}); res.OK || !strings.Contains(res.Detail, "certificate") {
		t.Fatalf("a different CA must not verify: %+v", res)
	}
	// control 2: the right CA but another server name does not verify either
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(x.ca.pem)
	tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "api.example.org"}, DialContext: func(ctx context.Context, n, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, n, x.f.httpsA)
	}}
	if _, err := (&http.Client{Transport: tr, Timeout: 3 * time.Second}).Get("https://api.example.org/"); err == nil {
		t.Fatal("a name the certificate does not carry must not verify")
	}
	// the body is exactly the address: no newline
	body := func() string {
		cli := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "api.ipify.org"}, DialContext: func(ctx context.Context, n, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, n, x.f.httpsA)
		}}, Timeout: 3 * time.Second}
		resp, err := cli.Get("https://api.ipify.org/")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}()
	if body != testSelf || strings.ContainsAny(body, "\r\n") {
		t.Fatalf("the body %q must be exactly %q", body, testSelf)
	}
}

// ---- the control API ------------------------------------------------------------------------------------------------------------------------------------

func TestAPIStateAndRefusals(t *testing.T) {
	x := startTestFx(t, "")
	var state map[string]any
	if x.get("/api/state", &state) != 200 || state["smtp_domains"].(float64) < 15 {
		t.Fatalf("state: %v", state)
	}
	if x.get("/readyz", nil) != 200 {
		t.Fatal("readyz")
	}
	for _, c := range []struct {
		path string
		body any
		want string
	}{
		{"/api/dns/modes", map[string]string{"name": "x.fixture.test", "mode": "explode"}, "mode"},
		{"/api/dns/modes", map[string]string{"nmae": "x"}, "unknown field"},
		{"/api/dns/records", map[string]any{"name": "x.fixture.test", "type": "A", "values": []string{"not-an-ip"}}, "not an IPv4"},
		{"/api/dns/records", map[string]any{"name": "x.fixture.test", "type": "CNAME", "values": []string{"y."}}, "not one of A, MX, TXT, PTR"},
		{"/api/dns/records", map[string]any{"name": "x.fixture.test", "type": "MX", "values": []string{"ten mx."}}, "preference"},
	} {
		if code, body := x.put(c.path, c.body); code != 400 || !strings.Contains(body, c.want) {
			t.Fatalf("PUT %s %v: %d %s, want 400 containing %q", c.path, c.body, code, body, c.want)
		}
	}
	if x.get("/api/dns/queries?since=-1", nil) != 400 {
		t.Fatal("a negative since must be a 400")
	}
	resp, err := http.Post("http://"+x.f.apiAddr+"/api/dns", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST on a GET path: %d", resp.StatusCode)
	}
}

// ---- the instrument itself -----------------------------------------------------------------------------------------------------------------------------

// TestSelfcheckPassesAgainstTheFixture runs the stage-1 instrument against the in-process fixture: every check is green and the count of checks is the one the file implies
// (a selfcheck that ran nothing is red).
func TestSelfcheckPassesAgainstTheFixture(t *testing.T) {
	x := startTestFx(t, "")
	e := checkEnv{dnsAddr: x.f.dnsAddr, dnsTCPAddr: x.f.dnsTCP, httpsAddr: x.f.httpsA, smtpAddr: x.f.smtpAddr, apiAddr: x.f.apiAddr, caPEM: x.ca.pem, selfIP: testSelf, nodeIP: testNode, cfg: x.cfg, dnsTimeout: 400 * time.Millisecond}
	res := runChecks(x.ctx(), e)
	var out bytes.Buffer
	if rc := printChecks(&out, res); rc != 0 {
		t.Fatalf("the selfcheck is red against the fixture it was written for:\n%s", out.String())
	}
	wantMin := len(x.cfg.Records) + len(x.cfg.Modes) + len(x.cfg.SMTP) // every record, every mode, at least one rule per SMTP domain
	if len(res) < wantMin {
		t.Fatalf("the selfcheck ran %d checks, the file implies at least %d:\n%s", len(res), wantMin, out.String())
	}
	if rc := printChecks(&bytes.Buffer{}, nil); rc != 1 {
		t.Fatal("a selfcheck that ran no check must be red")
	}
}

// TestSelfcheckFailsWhenTheFixtureIsWrong: the control of the instrument. Each way the live fixture can drift from the file turns exactly the matching check red, and the
// message names it.
func TestSelfcheckFailsWhenTheFixtureIsWrong(t *testing.T) {
	x := startTestFx(t, "")
	e := checkEnv{dnsAddr: x.f.dnsAddr, dnsTCPAddr: x.f.dnsTCP, httpsAddr: x.f.httpsA, smtpAddr: x.f.smtpAddr, apiAddr: x.f.apiAddr, caPEM: x.ca.pem, selfIP: testSelf, nodeIP: testNode, cfg: x.cfg, dnsTimeout: 400 * time.Millisecond}
	failed := func() map[string]string {
		out := map[string]string{}
		for _, r := range runChecks(x.ctx(), e) {
			if !r.OK {
				out[r.Name] = r.Detail
			}
		}
		return out
	}
	if f := failed(); len(f) != 0 {
		t.Fatalf("the control: an unmodified fixture must pass, failed: %v", f)
	}
	// a wrong A record
	x.put("/api/dns/records", map[string]any{"name": "verifier.fixture.test", "type": "A", "values": []string{"10.9.9.9"}})
	if f := failed(); len(f) != 1 || !strings.Contains(f["dns A verifier.fixture.test"], "10.9.9.9") {
		t.Fatalf("a changed A record must fail exactly its own check: %v", f)
	}
	x.put("/api/dns/records", map[string]any{"name": "verifier.fixture.test", "type": "A", "values": []string{testNode}})
	// a mode that answers when it must be silent
	x.put("/api/dns/modes", map[string]string{"name": "silent.fixture.test", "mode": "normal"})
	if f := failed(); len(f) != 1 || !strings.Contains(f["dns mode silent silent.fixture.test"], "must stay silent") {
		t.Fatalf("a silent name that answers must fail its check: %v", f)
	}
	x.put("/api/dns/modes", map[string]string{"name": "silent.fixture.test", "mode": "silent"})
	// a wrong SMTP reply
	x.put("/api/smtp/domains", map[string]any{"domain": "full.fixture.test", "rules": []map[string]string{{"match": "nx-*", "reply": "550 5.1.1 <{addr}> User unknown"}, {"match": "*", "reply": "250 2.1.5 OK"}}})
	if f := failed(); len(f) != 1 || !strings.Contains(f[`smtp full.fixture.test rule "*"`], "552") {
		t.Fatalf("a changed SMTP reply must fail exactly its own check: %v", f)
	}
	// a wrong CA
	other := newTestCA(t, "api.ipify.org")
	e.caPEM = other.pem
	x.put("/api/smtp/domains", map[string]any{"domain": "full.fixture.test", "rules": []map[string]string{{"match": "nx-*", "reply": "550 5.1.1 <{addr}> User unknown"}, {"match": "*", "reply": "552 5.2.2 Mailbox full"}}})
	if f := failed(); len(f) != 1 || f["https api.ipify.org"] == "" {
		t.Fatalf("a CA that does not sign the endpoint must fail the https check alone: %v", f)
	}
}
