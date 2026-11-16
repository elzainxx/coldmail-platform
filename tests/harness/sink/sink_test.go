package main

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/smtp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests run in the `go` workflow (no build tag, no database, no Docker). They talk to the real SMTP state machine and the real control API over loopback
// sockets, with a hand-rolled client (exact reply codes) and, where goroutines are needed, with net/smtp (a client the sink does not control). Each test that
// proves a mode BITES has its paired control: the same flow with the mode off, which must behave the other way.

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// ---- the sink under test ----

type testSink struct {
	srv  *Server
	st   *Store
	addr string // SMTP, host:port on loopback
	api  *httptest.Server
}

func testCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate a key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "smtp-ctl"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"smtp-ctl", "localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create a certificate: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// startSink runs a sink on loopback with STARTTLS and AUTH both required (the shape module 3's worker meets), a 64 KiB message cap and a silent log. mutate may change the Config.
func startSink(t *testing.T, mutate func(*Config)) *testSink {
	t.Helper()
	cfg := Config{
		Hostname:        "smtp-ctl.test",
		TLS:             &tls.Config{Certificates: []tls.Certificate{testCert(t)}, MinVersion: tls.VersionTLS12},
		RequireTLS:      true,
		RequireAuth:     true,
		MaxMessageBytes: 64 << 10,
		IdleTimeout:     20 * time.Second,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	st := newStore()
	srv := newServer(cfg, st)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(ln)
	}()
	for i := 0; !srv.Ready(); i++ {
		if i > 500 {
			t.Fatal("the SMTP server did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	api := httptest.NewServer(srv.APIHandler())
	t.Cleanup(func() {
		api.Close()
		_ = ln.Close()
		srv.Shutdown()
		<-done
	})
	return &testSink{srv: srv, st: st, addr: ln.Addr().String(), api: api}
}

func (s *testSink) get(t *testing.T, path string) (int, string) {
	t.Helper()
	resp, err := http.Get(s.api.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (s *testSink) do(t *testing.T, method, path, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, s.api.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// setMode posts a mode and requires it to be accepted.
func (s *testSink) setMode(t *testing.T, body string) Mode {
	t.Helper()
	code, out := s.do(t, http.MethodPost, "/api/mode", body)
	if code != http.StatusOK {
		t.Fatalf("POST /api/mode %s: HTTP %d: %s", body, code, out)
	}
	var m Mode
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("the answer to POST /api/mode is not a mode (%v): %s", err, out)
	}
	return m
}

// ---- a hand-rolled SMTP client (t.Fatalf: use it from the test goroutine only) ----

type smtpClient struct {
	t    *testing.T
	conn net.Conn
	br   *bufio.Reader
}

func dial(t *testing.T, addr string) *smtpClient {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial the sink: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &smtpClient{t: t, conn: conn, br: bufio.NewReader(conn)}
}

func (c *smtpClient) write(format string, a ...any) {
	c.t.Helper()
	_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := fmt.Fprintf(c.conn, format+"\r\n", a...); err != nil {
		c.t.Fatalf("write to the sink: %v", err)
	}
}

// reply reads one complete reply ("NNN-" lines until "NNN ") and returns its code and its text (the lines after the code, joined with \n).
func (c *smtpClient) reply() (int, string) {
	c.t.Helper()
	var lines []string
	for {
		_ = c.conn.SetReadDeadline(time.Now().Add(20 * time.Second))
		line, err := c.br.ReadString('\n')
		if err != nil {
			c.t.Fatalf("read a reply (lines so far %q): %v", lines, err)
		}
		line = strings.TrimRight(line, "\r\n")
		if len(line) < 4 {
			c.t.Fatalf("a reply line shorter than a code and a separator: %q", line)
		}
		code, err := strconv.Atoi(line[:3])
		if err != nil {
			c.t.Fatalf("a reply line that does not start with a code: %q", line)
		}
		lines = append(lines, line[4:])
		switch line[3] {
		case ' ':
			return code, strings.Join(lines, "\n")
		case '-':
		default:
			c.t.Fatalf("a reply line whose fourth byte is neither a space nor a hyphen: %q", line)
		}
	}
}

func (c *smtpClient) expect(code int, contains string) string {
	c.t.Helper()
	got, text := c.reply()
	if got != code || !strings.Contains(text, contains) {
		c.t.Fatalf("want a %d reply containing %q, got %d %q", code, contains, got, text)
	}
	return text
}

func (c *smtpClient) cmd(line string, code int, contains string) string {
	c.t.Helper()
	c.write("%s", line)
	return c.expect(code, contains)
}

func (c *smtpClient) startTLS() {
	c.t.Helper()
	c.cmd("STARTTLS", 220, "Ready to start TLS")
	tc := tls.Client(c.conn, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}) // the dev certificate is self-signed: module 3's worker skips validation for a private address as well
	_ = tc.SetDeadline(time.Now().Add(10 * time.Second))
	if err := tc.Handshake(); err != nil {
		c.t.Fatalf("TLS handshake with the sink: %v", err)
	}
	c.conn, c.br = tc, bufio.NewReader(tc)
}

// secure dials, reads the banner, says EHLO, upgrades and says EHLO again: the point at which module 3's worker would authenticate.
func (s *testSink) secure(t *testing.T) *smtpClient {
	t.Helper()
	c := dial(t, s.addr)
	c.expect(220, "ESMTP")
	c.cmd("EHLO client.example.test", 250, "STARTTLS")
	c.startTLS()
	c.cmd("EHLO client.example.test", 250, "AUTH PLAIN LOGIN")
	return c
}

// authed is secure() plus a successful AUTH PLAIN.
func (s *testSink) authed(t *testing.T) *smtpClient {
	t.Helper()
	c := s.secure(t)
	c.cmd("AUTH PLAIN "+b64("\x00sender@example.test\x00anything"), 235, "Authentication successful")
	return c
}

func message(subject, id string) string {
	return "From: Sender <sender@example.test>\r\nTo: someone@example.test\r\nSubject: " + subject + "\r\nMessage-ID: <" + id + ">\r\n\r\nhello\r\n"
}

// deliver runs MAIL, RCPT (one recipient, which must be accepted), DATA and the body; it returns the final reply.
func (c *smtpClient) deliver(from, to, body string) (int, string) {
	c.t.Helper()
	c.cmd("MAIL FROM:<"+from+">", 250, "OK")
	c.cmd("RCPT TO:<"+to+">", 250, "OK")
	c.cmd("DATA", 354, "End data")
	c.write("%s.", body)
	return c.reply()
}

// sendViaSMTP sends one message with net/smtp (STARTTLS, AUTH PLAIN, MAIL, RCPT, DATA): safe from any goroutine.
func sendViaSMTP(addr, to, subject string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	c, err := smtp.Dial(addr)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.Hello("client.example.test"); err != nil {
		return err
	}
	if err := c.StartTLS(&tls.Config{InsecureSkipVerify: true, ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
		return err
	}
	if err := c.Auth(smtp.PlainAuth("", "sender@example.test", "anything", host)); err != nil {
		return err
	}
	if err := c.Mail("sender@example.test"); err != nil {
		return err
	}
	if err := c.Rcpt(to); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := io.WriteString(w, message(subject, "net-smtp-"+to)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// waitFor polls cond every 10 ms for up to 10 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("not reached within 10s: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func addrsOf(msgs []Message) []string {
	var out []string
	for _, m := range msgs {
		out = append(out, m.To...)
	}
	sort.Strings(out)
	return out
}

// ---- the SMTP state machine ----

func TestSMTP_FullSessionRecordsTheMessage(t *testing.T) {
	s := startSink(t, nil)
	c := dial(t, s.addr)
	c.expect(220, "ESMTP")
	// before STARTTLS the sink offers STARTTLS and does NOT offer AUTH (a credential exchange in the clear)
	pre := c.cmd("EHLO mail.example.test", 250, "STARTTLS")
	if strings.Contains(pre, "AUTH") {
		t.Fatalf("EHLO before STARTTLS advertises AUTH: %q", pre)
	}
	c.startTLS()
	post := c.cmd("EHLO mail.example.test", 250, "AUTH PLAIN LOGIN")
	if strings.Contains(post, "STARTTLS") {
		t.Fatalf("EHLO after STARTTLS still advertises STARTTLS: %q", post)
	}
	c.cmd("AUTH PLAIN "+b64("\x00sender@example.test\x00anything"), 235, "Authentication successful")
	code, text := c.deliver("Sender@Example.Test", "Rcpt@Example.test", message("hello sink", "job-1@example.test"))
	if code != 250 || !strings.Contains(text, "queued as S") {
		t.Fatalf("the final reply is %d %q, want 250 naming the message (module 3's worker parses \"queued as <id>\")", code, text)
	}
	c.cmd("QUIT", 221, "Bye")

	msgs := s.st.Messages()
	if len(msgs) != 1 {
		t.Fatalf("the sink holds %d message(s), want 1", len(msgs))
	}
	m := msgs[0]
	if len(m.To) != 1 || m.To[0] != "rcpt@example.test" || m.EnvelopeFrom != "sender@example.test" {
		t.Fatalf("recorded envelope: to %v from %q, want [rcpt@example.test] from sender@example.test (addresses are lower-cased)", m.To, m.EnvelopeFrom)
	}
	if m.Subject != "hello sink" || m.MessageID != "<job-1@example.test>" || m.From != "Sender <sender@example.test>" {
		t.Fatalf("recorded headers: subject %q message-id %q from %q", m.Subject, m.MessageID, m.From)
	}
	if m.Seq != 1 || m.HeldMs != 0 || m.Size == 0 || time.Since(m.At) > time.Minute || time.Until(m.At) > time.Second {
		t.Fatalf("recorded bookkeeping is off: seq %d held %dms size %d at %s", m.Seq, m.HeldMs, m.Size, m.At)
	}

	// the API lists it with the fields the harness test reads, and never serves a body
	code2, body := s.get(t, "/api/messages")
	if code2 != http.StatusOK {
		t.Fatalf("GET /api/messages: HTTP %d", code2)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(body), &rows); err != nil || len(rows) != 1 {
		t.Fatalf("GET /api/messages is not a list of one message (%v): %s", err, body)
	}
	for _, k := range []string{"to", "subject", "message_id", "at", "from", "seq"} {
		if _, ok := rows[0][k]; !ok {
			t.Fatalf("a listed message has no %q: %s", k, body)
		}
	}
	if _, ok := rows[0]["body"]; ok {
		t.Fatalf("the listing serves a body: %s", body)
	}
}

func TestSMTP_AuthLoginAndPlainAcceptAnyCredential(t *testing.T) {
	s := startSink(t, nil)

	// LOGIN, step by step: the two challenges are "Username:" and "Password:" (what module 3's worker answers)
	c := s.secure(t)
	c.cmd("AUTH LOGIN", 334, b64("Username:"))
	c.cmd(b64("someone@example.test"), 334, b64("Password:"))
	c.cmd(b64("whatever-it-is"), 235, "Authentication successful")
	c.cmd("AUTH PLAIN "+b64("\x00a\x00b"), 503, "already authenticated")

	// LOGIN with the user name as an initial response
	c = s.secure(t)
	c.cmd("AUTH LOGIN "+b64("someone@example.test"), 334, b64("Password:"))
	c.cmd(b64("x"), 235, "")

	// PLAIN with and without an initial response
	c = s.secure(t)
	c.cmd("AUTH PLAIN", 334, "")
	c.cmd(b64("\x00user@example.test\x00pw"), 235, "")
	c = s.secure(t)
	c.cmd("AUTH plain "+b64("\x00user@example.test\x00pw"), 235, "") // the mechanism is case-insensitive

	// what is not a credential exchange is refused, and the refusal does not authenticate the session
	c = s.secure(t)
	c.cmd("AUTH PLAIN !!!not-base64!!!", 501, "cannot decode")
	c.cmd("AUTH CRAM-MD5", 504, "unrecognized authentication type")
	c.cmd("AUTH PLAIN", 334, "")
	c.cmd("*", 501, "cancelled")
	c.cmd("MAIL FROM:<a@example.test>", 530, "Authentication required")
	c.cmd("AUTH LOGIN", 334, b64("Username:"))
	c.cmd("*", 501, "cancelled")
	c.cmd("AUTH PLAIN "+b64("\x00u\x00p"), 235, "") // and the same session can still authenticate
}

func TestSMTP_StartTLSIsRequiredBeforeAuthAndMail(t *testing.T) {
	s := startSink(t, nil)
	c := dial(t, s.addr)
	c.expect(220, "ESMTP")
	c.cmd("STARTTLS", 503, "send EHLO first")
	c.cmd("EHLO client.example.test", 250, "STARTTLS")
	c.cmd("AUTH PLAIN "+b64("\x00u\x00p"), 530, "STARTTLS")
	c.cmd("MAIL FROM:<a@example.test>", 530, "STARTTLS")
	c.cmd("STARTTLS now", 501, "no parameters")
	c.startTLS()
	// the session starts over after the handshake (RFC 3207): MAIL without a new EHLO is out of sequence, and without AUTH it is refused
	c.cmd("MAIL FROM:<a@example.test>", 503, "EHLO")
	c.cmd("EHLO client.example.test", 250, "AUTH")
	c.cmd("MAIL FROM:<a@example.test>", 530, "Authentication required")
	c.cmd("STARTTLS", 503, "already active")

	// STARTTLS pipelined behind the command is the plaintext-injection attack: the upgrade is refused and the connection dropped
	c2 := dial(t, s.addr)
	c2.expect(220, "ESMTP")
	c2.cmd("EHLO client.example.test", 250, "STARTTLS")
	c2.write("STARTTLS\r\nMAIL FROM:<injected@example.test>")
	got, text := c2.reply()
	if got != 501 || !strings.Contains(text, "data after STARTTLS") {
		t.Fatalf("a STARTTLS with a command pipelined behind it answered %d %q, want 501 data after STARTTLS", got, text)
	}

	// the control: a sink with neither TLS nor the requirements takes the same dialogue in the clear (so the refusals above are the requirement and not a broken sink), and does not offer STARTTLS
	plain := startSink(t, func(cfg *Config) { cfg.TLS, cfg.RequireTLS, cfg.RequireAuth = nil, false, false })
	p := dial(t, plain.addr)
	p.expect(220, "ESMTP")
	ehlo := p.cmd("EHLO client.example.test", 250, "AUTH PLAIN LOGIN")
	if strings.Contains(ehlo, "STARTTLS") {
		t.Fatalf("a sink without a certificate advertises STARTTLS: %q", ehlo)
	}
	p.cmd("STARTTLS", 454, "TLS not available")
	if code, _ := p.deliver("a@example.test", "b@example.test", message("clear", "clear-1@example.test")); code != 250 {
		t.Fatalf("the control sink refused a message in the clear: %d", code)
	}
}

func TestSMTP_CommandOrderIsEnforced(t *testing.T) {
	s := startSink(t, nil)

	fresh := dial(t, s.addr)
	fresh.expect(220, "ESMTP")
	fresh.cmd("MAIL FROM:<a@example.test>", 503, "EHLO")
	fresh.cmd("AUTH PLAIN "+b64("\x00u\x00p"), 503, "EHLO")
	fresh.cmd("EHLO", 501, "domain")
	fresh.cmd("NOOP", 250, "OK")

	c := s.authed(t)
	c.cmd("RCPT TO:<b@example.test>", 503, "MAIL")
	c.cmd("DATA", 503, "MAIL")
	c.cmd("MAIL FROM:nobrackets@example.test", 501, "syntax")
	c.cmd("MAIL FROM:<a@example.test> SIZE=99999999", 552, "size")
	c.cmd("MAIL FROM:<a@example.test>", 250, "OK")
	c.cmd("MAIL FROM:<a@example.test>", 503, "nested")
	c.cmd("DATA", 503, "RCPT")
	c.cmd("RCPT TO:<>", 501, "syntax")
	c.cmd("RCPT TO:<b@example.test>", 250, "OK")
	c.cmd("RSET", 250, "OK")
	c.cmd("RCPT TO:<b@example.test>", 503, "MAIL") // RSET cleared the transaction
	c.cmd("NOOP", 250, "OK")
	c.cmd("VRFY someone", 252, "")
	c.cmd("HELP", 214, "")
	c.cmd("FOO bar", 502, "not recognized")
	c.write("%s", "")
	c.expect(500, "empty command")
	c.write("%s", strings.Repeat("A", maxCommandLine+500)) // the sink discards a line that long and the session goes on
	c.expect(500, "too long")
	c.cmd("NOOP", 250, "OK")

	// a delivery after all the refusals still works
	if code, _ := c.deliver("a@example.test", "b@example.test", message("order", "order-1@example.test")); code != 250 {
		t.Fatalf("a delivery after the out-of-sequence commands answered %d, want 250", code)
	}
	// a second transaction on the same connection
	if code, _ := c.deliver("a@example.test", "c@example.test", message("order", "order-2@example.test")); code != 250 {
		t.Fatalf("the second transaction on one connection answered %d, want 250", code)
	}
	if got, want := strings.Join(addrsOf(s.st.Messages()), ","), "b@example.test,c@example.test"; got != want {
		t.Fatalf("the sink holds messages to %s, want %s", got, want)
	}
}

func TestSMTP_DotStuffingLongLinesAndTheSizeCap(t *testing.T) {
	s := startSink(t, nil)
	c := s.authed(t)

	// dot-stuffing is undone exactly once, at the start of a line; a long line that crosses the reader's buffer is not "a line starting with a dot" at its second chunk
	long := strings.Repeat("a", 4096) + ".tail"
	dotted := "." + strings.Repeat("y", 5000)
	body := "Subject: dots\r\n\r\n..one\r\n...two\r\na.b\r\n" + long + "\r\n." + dotted + "\r\nlast\r\n"
	if code, text := c.deliver("a@example.test", "dots@example.test", body); code != 250 {
		t.Fatalf("a message with dot-stuffed and long lines answered %d %q", code, text)
	}
	msgs := s.st.Messages()
	if len(msgs) != 1 {
		t.Fatalf("the sink holds %d message(s), want 1", len(msgs))
	}
	want := "Subject: dots\r\n\r\n.one\r\n..two\r\na.b\r\n" + long + "\r\n" + dotted + "\r\nlast\r\n"
	if got := string(msgs[0].body); got != want {
		t.Fatalf("the stored body is not the message that was sent:\n got %d bytes %.80q...\nwant %d bytes %.80q...", len(got), got, len(want), want)
	}
	if msgs[0].Subject != "dots" {
		t.Fatalf("subject %q, want dots", msgs[0].Subject)
	}

	// over the cap (64 KiB here): the message is read to its end and refused, nothing is recorded, and the SAME connection takes the next one
	c.cmd("MAIL FROM:<a@example.test>", 250, "OK")
	c.cmd("RCPT TO:<big@example.test>", 250, "OK")
	c.cmd("DATA", 354, "End data")
	for i := 0; i < 80; i++ {
		c.write("%s", strings.Repeat("x", 1000))
	}
	c.write("%s", ".")
	c.expect(552, "too big")
	if n := len(s.st.Messages()); n != 1 {
		t.Fatalf("the sink holds %d message(s) after an over-sized one, want still 1", n)
	}
	if code, _ := c.deliver("a@example.test", "after@example.test", message("after", "after-1@example.test")); code != 250 {
		t.Fatalf("a message after an over-sized one answered %d, want 250", code)
	}
	if got, want := strings.Join(addrsOf(s.st.Messages()), ","), "after@example.test,dots@example.test"; got != want {
		t.Fatalf("the sink holds messages to %s, want %s", got, want)
	}
}

// ---- the modes ----

func TestMode_RejectRcptAnswersAtRcptAndRecordsNothing(t *testing.T) {
	s := startSink(t, nil)
	s.setMode(t, `{"reject_rcpt":["Bad@Example.test","bad2@example.test"]}`)

	c := s.authed(t)
	c.cmd("MAIL FROM:<sender@example.test>", 250, "OK")
	c.cmd("RCPT TO:<bad@example.test>", 550, "5.1.1 no such user") // compared lower-cased
	c.cmd("RCPT TO:<good@example.test>", 250, "OK")                // a reject does not break the session
	c.cmd("DATA", 354, "End data")
	c.write("%s.", message("only the good one", "rej-1@example.test"))
	c.expect(250, "queued as")

	// a transaction whose every recipient was rejected has no DATA
	c.cmd("MAIL FROM:<sender@example.test>", 250, "OK")
	c.cmd("RCPT TO:<bad2@example.test>", 550, "no such user")
	c.cmd("DATA", 554, "no valid recipients")

	if got := strings.Join(addrsOf(s.st.Messages()), ","); got != "good@example.test" {
		t.Fatalf("the sink holds messages to %q, want only good@example.test (a rejected recipient never reaches the sink)", got)
	}

	// the reject code is a dial, and a 4xx code reads 4.1.1
	s.setMode(t, `{"reject_rcpt":["bad@example.test"],"reject_code":553}`)
	c.cmd("MAIL FROM:<sender@example.test>", 250, "OK")
	c.cmd("RCPT TO:<bad@example.test>", 553, "5.1.1 no such user")
	s.setMode(t, `{"reject_rcpt":["bad@example.test"],"reject_code":452}`)
	c.cmd("RCPT TO:<bad@example.test>", 452, "4.1.1 no such user")

	// the control: with the mode off the same recipient is accepted and delivered
	s.setMode(t, `{}`)
	c.cmd("RCPT TO:<bad@example.test>", 250, "OK")
	c.cmd("DATA", 354, "End data")
	c.write("%s.", message("now accepted", "rej-2@example.test"))
	c.expect(250, "queued as")
	if got := strings.Join(addrsOf(s.st.Messages()), ","); got != "bad@example.test,good@example.test" {
		t.Fatalf("after the mode was cleared the sink holds messages to %q, want bad@ and good@", got)
	}
}

func TestMode_DeferRcptIsTemporaryAndRelents(t *testing.T) {
	s := startSink(t, nil)
	s.setMode(t, `{"defer_rcpt":["later@example.test"]}`)
	c := s.authed(t)
	for i := 0; i < 2; i++ { // a retry meets the same answer
		c.cmd("MAIL FROM:<sender@example.test>", 250, "OK")
		c.cmd("RCPT TO:<later@example.test>", 451, "4.7.1 try later")
		c.cmd("RSET", 250, "OK")
	}
	if n := len(s.st.Messages()); n != 0 {
		t.Fatalf("a deferred recipient reached the sink (%d message(s))", n)
	}
	// the control, and the point of a deferral: it relents
	s.setMode(t, `{}`)
	if code, _ := c.deliver("sender@example.test", "later@example.test", message("at last", "defer-1@example.test")); code != 250 {
		t.Fatalf("after the deferral was removed the recipient answered %d, want 250", code)
	}
	counts := s.st.Rcpt()["later@example.test"]
	if counts.Deferred != 2 || counts.Accepted != 1 || counts.Rejected != 0 {
		t.Fatalf("recipient counters %+v, want deferred 2, accepted 1, rejected 0", counts)
	}
	// a reject beats a deferral for the same address
	s.setMode(t, `{"reject_rcpt":["both@example.test"],"defer_rcpt":["both@example.test"]}`)
	c.cmd("MAIL FROM:<sender@example.test>", 250, "OK")
	c.cmd("RCPT TO:<both@example.test>", 550, "no such user")
}

func TestMode_DataDelayHoldsTheFinalReply(t *testing.T) {
	s := startSink(t, nil)
	s.setMode(t, `{"data_delay_ms":800}`)
	c := s.authed(t)
	c.cmd("MAIL FROM:<sender@example.test>", 250, "OK")
	c.cmd("RCPT TO:<slow@example.test>", 250, "OK")
	c.cmd("DATA", 354, "End data")
	start := time.Now()
	c.write("%s.", message("held", "held-1@example.test"))

	// while the reply is held the session is "in flight": counted, listed with its recipient, and the message is NOT in the sink yet
	waitFor(t, "the held session shows in /api/inflight", func() bool { return s.st.Inflight().Inflight == 1 })
	v := s.st.Inflight()
	if len(v.Held) != 1 || len(v.Held[0].To) != 1 || v.Held[0].To[0] != "slow@example.test" || v.Held[0].RemainingMs > 800 || v.Peak != 1 || v.HeldTotal != 1 {
		t.Fatalf("while held: %+v, want one row to slow@example.test, remaining <= 800ms, peak 1, held_total 1", v)
	}
	if n := len(s.st.Messages()); n != 0 {
		t.Fatalf("the sink holds %d message(s) before the held reply was sent: a message is accepted when the 250 goes out", n)
	}

	c.expect(250, "queued as")
	if el := time.Since(start); el < 780*time.Millisecond {
		t.Fatalf("the final reply arrived %s after the end-of-data, want it held for 800ms", el)
	}
	v = s.st.Inflight()
	if v.Inflight != 0 || v.Peak != 1 || v.HeldTotal != 1 || len(v.Held) != 0 {
		t.Fatalf("after the reply: %+v, want nothing held, peak 1, held_total 1", v)
	}
	msgs := s.st.Messages()
	if len(msgs) != 1 || msgs[0].HeldMs < 800 {
		t.Fatalf("after the reply the sink holds %+v, want one message held for at least 800ms", msgs)
	}

	// the control: with the delay removed the next message is not held (nothing counted, held_ms 0)
	s.setMode(t, `{}`)
	if code, _ := c.deliver("sender@example.test", "quick@example.test", message("quick", "held-2@example.test")); code != 250 {
		t.Fatalf("the undelayed message answered %d", code)
	}
	msgs = s.st.Messages()
	if len(msgs) != 2 || msgs[1].HeldMs != 0 || s.st.Inflight().HeldTotal != 1 {
		t.Fatalf("after the control: %+v / %+v, want the second message not held and held_total still 1", msgs, s.st.Inflight())
	}
}

func TestMode_DelayRecipientsLimitsTheDelay(t *testing.T) {
	s := startSink(t, nil)
	s.setMode(t, `{"data_delay_ms":300,"delay_recipients":["slow@example.test"]}`)
	c := s.authed(t)
	if code, _ := c.deliver("sender@example.test", "fast@example.test", message("fast", "dr-1@example.test")); code != 250 {
		t.Fatalf("answered %d", code)
	}
	if v := s.st.Inflight(); v.HeldTotal != 0 || v.Peak != 0 {
		t.Fatalf("a recipient that is not named was held: %+v", v)
	}
	if code, _ := c.deliver("sender@example.test", "slow@example.test", message("slow", "dr-2@example.test")); code != 250 {
		t.Fatalf("answered %d", code)
	}
	msgs := s.st.Messages()
	if len(msgs) != 2 || msgs[0].To[0] != "fast@example.test" || msgs[0].HeldMs != 0 || msgs[1].To[0] != "slow@example.test" || msgs[1].HeldMs < 300 {
		t.Fatalf("held_ms by recipient: %+v, want fast 0 and slow >= 300", msgs)
	}
	if v := s.st.Inflight(); v.HeldTotal != 1 || v.Peak != 1 {
		t.Fatalf("after both: %+v, want exactly one held session", v)
	}
}

func TestMode_FailAuthAnswers535AndRelents(t *testing.T) {
	s := startSink(t, nil)
	s.setMode(t, `{"fail_auth":true}`)
	c := s.secure(t)
	c.cmd("AUTH PLAIN "+b64("\x00u\x00p"), 535, "5.7.8")
	c.cmd("AUTH LOGIN", 334, b64("Username:"))
	c.cmd(b64("u"), 334, b64("Password:"))
	c.cmd(b64("p"), 535, "5.7.8")
	c.cmd("MAIL FROM:<a@example.test>", 530, "Authentication required") // a refused AUTH leaves the session unauthenticated
	s.setMode(t, `{}`)
	c.cmd("AUTH PLAIN "+b64("\x00u\x00p"), 235, "Authentication successful") // the control: the same session authenticates once the mode is off
}

// ---- the control API ----

func TestAPI_ModeIsReplacedAsAWholeAndValidated(t *testing.T) {
	s := startSink(t, nil)

	code, body := s.get(t, "/api/mode")
	if code != http.StatusOK {
		t.Fatalf("GET /api/mode: HTTP %d", code)
	}
	for _, frag := range []string{`"reject_rcpt":[]`, `"reject_code":550`, `"defer_rcpt":[]`, `"data_delay_ms":0`, `"delay_recipients":[]`, `"fail_auth":false`} {
		if !strings.Contains(body, frag) {
			t.Fatalf("the default mode lacks %s: %s", frag, body)
		}
	}

	// lower-cased and de-duplicated, in the order given
	m := s.setMode(t, `{"reject_rcpt":["B@Example.test"," a@example.test","b@example.test"],"reject_code":551,"data_delay_ms":1500,"delay_recipients":["Z@example.test"],"fail_auth":true}`)
	if strings.Join(m.RejectRcpt, ",") != "b@example.test,a@example.test" || m.RejectCode != 551 || m.DataDelayMs != 1500 || strings.Join(m.DelayRecipients, ",") != "z@example.test" || !m.FailAuth || len(m.DeferRcpt) != 0 {
		t.Fatalf("the stored mode is %+v", m)
	}

	// a POST replaces the whole mode: what the body does not name goes back to its default
	m = s.setMode(t, `{"defer_rcpt":["x@example.test"]}`)
	if len(m.RejectRcpt) != 0 || m.RejectCode != 550 || m.DataDelayMs != 0 || len(m.DelayRecipients) != 0 || m.FailAuth || strings.Join(m.DeferRcpt, ",") != "x@example.test" {
		t.Fatalf("a POST did not replace the whole mode: %+v", m)
	}
	_, before := s.get(t, "/api/mode")

	// what is invalid is refused with 400 and changes nothing
	bad := map[string]string{
		"an unknown field (a misspelling must not read as no change)": `{"reject_rcpts":["a@example.test"]}`,
		"a reject code below 400":                                     `{"reject_code":250}`,
		"a reject code above 599":                                     `{"reject_code":600}`,
		"a negative delay":                                            `{"data_delay_ms":-1}`,
		"a delay above the limit":                                     `{"data_delay_ms":120001}`,
		"an address with a space":                                     `{"reject_rcpt":["a b@example.test"]}`,
		"an address in angle brackets":                                `{"defer_rcpt":["<a@example.test>"]}`,
		"an empty address":                                            `{"delay_recipients":[""]}`,
		"a wrong type":                                                `{"data_delay_ms":"slow"}`,
		"text that is not JSON":                                       `reject everything`,
		"an empty body":                                               ``,
		"two JSON values":                                             `{} {}`,
	}
	for what, req := range bad {
		code, out := s.do(t, http.MethodPost, "/api/mode", req)
		if code != http.StatusBadRequest || !strings.Contains(out, `"error"`) {
			t.Errorf("%s: POST %q answered HTTP %d %q, want 400 with an error", what, req, code, out)
		}
		if _, after := s.get(t, "/api/mode"); after != before {
			t.Errorf("%s: the refused POST changed the mode: %s -> %s", what, before, after)
		}
	}
	// the control of the refusals: a valid body is accepted right after them
	if m := s.setMode(t, `{"reject_code":599,"data_delay_ms":120000}`); m.RejectCode != 599 || m.DataDelayMs != 120000 {
		t.Fatalf("the limits themselves are valid, got %+v", m)
	}
}

func TestAPI_MessagesResetReadyzAndMethods(t *testing.T) {
	s := startSink(t, nil)

	if code, body := s.get(t, "/readyz"); code != http.StatusOK || strings.TrimSpace(body) != "ok" {
		t.Fatalf("GET /readyz: HTTP %d %q, want 200 ok", code, body)
	}
	// the control of readyz: a server that is not accepting connections is not ready
	idle := newServer(Config{}, newStore())
	rec := httptest.NewRecorder()
	idle.APIHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("a server that has not started serving answered /readyz with %d, want 503", rec.Code)
	}

	if code, body := s.get(t, "/api/messages"); code != http.StatusOK || strings.TrimSpace(body) != "[]" {
		t.Fatalf("an empty sink lists %q (HTTP %d), want []", body, code)
	}
	c := s.authed(t)
	if code, _ := c.deliver("a@example.test", "m@example.test", message("one", "api-1@example.test")); code != 250 {
		t.Fatalf("answered %d", code)
	}
	s.setMode(t, `{"reject_rcpt":["x@example.test"],"data_delay_ms":50}`)
	c.cmd("MAIL FROM:<a@example.test>", 250, "OK")
	c.cmd("RCPT TO:<x@example.test>", 550, "")
	if n := len(s.st.Messages()); n != 1 {
		t.Fatalf("the sink holds %d message(s), want 1", n)
	}

	code, body := s.do(t, http.MethodPost, "/api/reset", "")
	if code != http.StatusOK || !strings.Contains(body, `"cleared":1`) {
		t.Fatalf("POST /api/reset: HTTP %d %q, want 200 with cleared 1", code, body)
	}
	if code, body := s.get(t, "/api/messages"); code != http.StatusOK || strings.TrimSpace(body) != "[]" {
		t.Fatalf("after a reset the sink lists %q, want []", body)
	}
	if m := s.st.Mode(); len(m.RejectRcpt) != 0 || m.DataDelayMs != 0 || m.RejectCode != 550 {
		t.Fatalf("a reset left the mode %+v, want the default", m)
	}
	if rc := s.st.Rcpt(); len(rc) != 0 {
		t.Fatalf("a reset left recipient counters %+v", rc)
	}

	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{http.MethodPost, "/api/messages", http.StatusMethodNotAllowed},
		{http.MethodGet, "/api/reset", http.StatusMethodNotAllowed},
		{http.MethodDelete, "/api/mode", http.StatusMethodNotAllowed},
		{http.MethodPost, "/api/inflight", http.StatusMethodNotAllowed},
		{http.MethodGet, "/api/nothing", http.StatusNotFound},
		{http.MethodGet, "/", http.StatusNotFound},
	} {
		if code, _ := s.do(t, tc.method, tc.path, ""); code != tc.want {
			t.Errorf("%s %s answered HTTP %d, want %d", tc.method, tc.path, code, tc.want)
		}
	}
}

func TestAPI_InflightShowsHeldSessionsAndThePeak(t *testing.T) {
	s := startSink(t, nil)
	s.setMode(t, `{"data_delay_ms":1500}`)

	errs := make(chan error, 2)
	send := func(to string) { errs <- sendViaSMTP(s.addr, to, "held "+to) }
	go send("one@example.test")
	waitFor(t, "the first session is held", func() bool { return s.st.Inflight().Inflight == 1 })
	go send("two@example.test")
	waitFor(t, "both sessions are held at once", func() bool { return s.st.Inflight().Inflight == 2 })

	code, body := s.get(t, "/api/inflight")
	if code != http.StatusOK {
		t.Fatalf("GET /api/inflight: HTTP %d", code)
	}
	var v InflightView
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("GET /api/inflight is not the documented shape (%v): %s", err, body)
	}
	if v.Peak < 2 || len(v.Held) != v.Inflight {
		t.Fatalf("inflight view %+v, want peak >= 2 and one row per held session", v)
	}
	var to []string
	for _, h := range v.Held {
		to = append(to, h.To...)
		if h.RemainingMs < 0 || h.RemainingMs > 1500 || h.AgeMs < 0 {
			t.Fatalf("a held row has remaining %dms / age %dms (the hold is 1500ms): %+v", h.RemainingMs, h.AgeMs, h)
		}
	}
	sort.Strings(to)
	if v.Inflight == 2 && strings.Join(to, ",") != "one@example.test,two@example.test" {
		t.Fatalf("the held rows name %v, want both recipients", to)
	}

	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("a held send failed: %v", err)
		}
	}
	v = s.st.Inflight()
	if v.Inflight != 0 || v.Peak != 2 || v.HeldTotal != 2 || len(s.st.Messages()) != 2 {
		t.Fatalf("after both finished: %+v with %d message(s), want nothing held, peak 2, held_total 2, 2 messages", v, len(s.st.Messages()))
	}
	// a reset forgets the peak
	if code, _ := s.do(t, http.MethodPost, "/api/reset", ""); code != http.StatusOK {
		t.Fatalf("reset: HTTP %d", code)
	}
	if v := s.st.Inflight(); v.Peak != 0 || v.HeldTotal != 0 {
		t.Fatalf("a reset left %+v, want peak 0 and held_total 0", v)
	}
}

func TestAPI_RcptCountersPerRecipient(t *testing.T) {
	s := startSink(t, nil)
	s.setMode(t, `{"reject_rcpt":["a@example.test"],"defer_rcpt":["b@example.test"]}`)
	c := s.authed(t)
	for i := 0; i < 2; i++ {
		c.cmd("MAIL FROM:<sender@example.test>", 250, "OK")
		c.cmd("RCPT TO:<a@example.test>", 550, "")
		c.cmd("RCPT TO:<b@example.test>", 451, "")
		c.cmd("RCPT TO:<c@example.test>", 250, "")
		c.cmd("RSET", 250, "")
	}
	code, body := s.get(t, "/api/rcpt")
	if code != http.StatusOK {
		t.Fatalf("GET /api/rcpt: HTTP %d", code)
	}
	var got map[string]RcptCount
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("GET /api/rcpt is not a map of counters (%v): %s", err, body)
	}
	want := map[string]RcptCount{"a@example.test": {Rejected: 2}, "b@example.test": {Deferred: 2}, "c@example.test": {Accepted: 2}}
	if len(got) != len(want) {
		t.Fatalf("counters %+v, want %+v", got, want)
	}
	for a, w := range want {
		if got[a] != w {
			t.Fatalf("counters for %s are %+v, want %+v", a, got[a], w)
		}
	}
	s.do(t, http.MethodPost, "/api/reset", "")
	if _, body := s.get(t, "/api/rcpt"); strings.TrimSpace(body) != "{}" {
		t.Fatalf("after a reset the counters are %q, want {}", body)
	}
}

// ---- concurrency, shutdown and the pieces ----

func TestSMTP_ConcurrentSessionsRecordEveryMessageOnce(t *testing.T) {
	s := startSink(t, nil)
	const n = 24
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- sendViaSMTP(s.addr, fmt.Sprintf("r%02d@example.test", i), fmt.Sprintf("message %d", i))
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("a concurrent send failed: %v", err)
		}
	}
	msgs := s.st.Messages()
	if len(msgs) != n {
		t.Fatalf("the sink holds %d message(s), want %d", len(msgs), n)
	}
	seenTo := map[string]int{}
	seenSeq := map[int64]bool{}
	for _, m := range msgs {
		for _, a := range m.To {
			seenTo[a]++
		}
		seenSeq[m.Seq] = true
	}
	for i := 0; i < n; i++ {
		if a := fmt.Sprintf("r%02d@example.test", i); seenTo[a] != 1 {
			t.Fatalf("%s is in the sink %d times, want exactly once", a, seenTo[a])
		}
	}
	if len(seenSeq) != n {
		t.Fatalf("%d distinct sequence numbers for %d messages", len(seenSeq), n)
	}
}

func TestSMTP_ShutdownReleasesAHeldSessionWithoutAnAcceptance(t *testing.T) {
	s := startSink(t, nil)
	s.setMode(t, `{"data_delay_ms":60000}`)
	errc := make(chan error, 1)
	go func() { errc <- sendViaSMTP(s.addr, "held@example.test", "never accepted") }()
	waitFor(t, "the session is held", func() bool { return s.st.Inflight().Inflight == 1 })

	s.srv.Shutdown()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("the client of a held session was told the message was accepted although the sink shut down")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the held session was not released by Shutdown")
	}
	if n := len(s.st.Messages()); n != 0 {
		t.Fatalf("the sink holds %d message(s) after shutting down during a hold, want none: nothing was accepted", n)
	}
	if v := s.st.Inflight(); v.Inflight != 0 {
		t.Fatalf("%d session(s) still counted as held after Shutdown", v.Inflight)
	}
}

func TestStore_QueuedAsIdsStayUniqueAcrossAReset(t *testing.T) {
	st := newStore()
	seen := map[string]bool{}
	for round := 0; round < 2; round++ {
		for i := 0; i < 3; i++ {
			_, id, err := st.record(Message{To: []string{"a@example.test"}})
			if err != nil {
				t.Fatal(err)
			}
			if seen[id] {
				t.Fatalf("the id %s was handed out twice (module 3 stores it under a UNIQUE constraint)", id)
			}
			seen[id] = true
		}
		if n := st.Reset(); n != 3 {
			t.Fatalf("Reset forgot %d message(s), want 3", n)
		}
	}
	if len(seen) != 6 {
		t.Fatalf("%d distinct ids, want 6", len(seen))
	}
	// after a reset the sequence numbers start over (they number the messages in the sink, not the ids)
	m, _, _ := st.record(Message{})
	if m.Seq != 1 {
		t.Fatalf("the first message after a reset has seq %d, want 1", m.Seq)
	}
}

func TestParseHeaders(t *testing.T) {
	cases := []struct {
		name                     string
		raw                      string
		from, subject, messageID string
	}{
		{"crlf", "From: A <a@example.test>\r\nSubject: hi\r\nMessage-ID: <1@example.test>\r\n\r\nbody", "A <a@example.test>", "hi", "<1@example.test>"},
		{"lf only", "From: a@example.test\nSubject: hi\nMessage-Id: <2@example.test>\n\nbody", "a@example.test", "hi", "<2@example.test>"},
		{"folded subject", "Subject: one\r\n two\r\n\tthree\r\nFrom: a@example.test\r\n\r\n", "a@example.test", "one two three", ""},
		{"rfc 2047 subject", "Subject: =?UTF-8?Q?caf=C3=A9?=\r\n\r\n", "", "café", ""},
		{"the first header wins", "Subject: first\r\nSubject: second\r\n\r\n", "", "first", ""},
		{"a header in the body is not a header", "From: a@example.test\r\n\r\nSubject: in the body\r\nMessage-ID: <x@example.test>\r\n", "a@example.test", "", ""},
		{"no headers at all", "just text", "", "", ""},
	}
	for _, tc := range cases {
		from, subject, id := parseHeaders([]byte(tc.raw))
		if from != tc.from || subject != tc.subject || id != tc.messageID {
			t.Errorf("%s: got from %q subject %q message-id %q, want %q %q %q", tc.name, from, subject, id, tc.from, tc.subject, tc.messageID)
		}
	}
}

func TestMode_NormalizeRejectsWhatIsInvalid(t *testing.T) {
	good := defaultMode()
	if err := good.normalize(); err != nil {
		t.Fatalf("the default mode is invalid: %v", err)
	}
	tooMany := defaultMode()
	for i := 0; i <= maxListEntries; i++ {
		tooMany.RejectRcpt = append(tooMany.RejectRcpt, fmt.Sprintf("a%d@example.test", i))
	}
	longAddr := defaultMode()
	longAddr.DeferRcpt = []string{strings.Repeat("a", maxAddrLen+1)}
	nilLists := Mode{RejectCode: 550}
	for _, tc := range []struct {
		name string
		m    Mode
		want string // a fragment of the error; "" = valid
	}{
		{"code 399", Mode{RejectCode: 399}, "reject_code"},
		{"code 0 (a body that names no code is given the default before it gets here)", Mode{RejectCode: 0}, "reject_code"},
		{"delay -5", Mode{RejectCode: 550, DataDelayMs: -5}, "data_delay_ms"},
		{"delay over the limit", Mode{RejectCode: 550, DataDelayMs: maxDelayMs + 1}, "data_delay_ms"},
		{"a comma in an address", Mode{RejectCode: 550, RejectRcpt: []string{"a@example.test,b@example.test"}}, "reject_rcpt"},
		{"an empty address", Mode{RejectCode: 550, DelayRecipients: []string{" "}}, "delay_recipients"},
		{"too many entries", tooMany, "limit"},
		{"an address that is too long", longAddr, "longer"},
		{"nil lists are valid (and become empty lists)", nilLists, ""},
	} {
		m := tc.m
		err := m.normalize()
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: refused: %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: error %v, want one naming %q", tc.name, err, tc.want)
		}
		if tc.want == "" && (m.RejectRcpt == nil || m.DeferRcpt == nil || m.DelayRecipients == nil) {
			t.Errorf("%s: normalize left a nil list", tc.name)
		}
	}
}

func TestSMTP_ReplyDelayPausesEveryReply(t *testing.T) {
	const pause = 60 * time.Millisecond
	s := startSink(t, func(cfg *Config) { cfg.ReplyDelay = pause })
	c := dial(t, s.addr)
	start := time.Now()
	c.expect(220, "ESMTP")
	if el := time.Since(start); el < pause-5*time.Millisecond {
		t.Fatalf("the banner came %s after the connection, want it paused for %s", el, pause)
	}
	for _, line := range []string{"EHLO client.example.test", "NOOP", "RSET"} {
		start = time.Now()
		c.cmd(line, 250, "")
		if el := time.Since(start); el < pause-5*time.Millisecond {
			t.Fatalf("the reply to %q came %s after the command, want it paused for %s", line, el, pause)
		}
	}
	// the control: a Config that sets no pause has none (the other tests, which time their holds, run without one)
	if d := newServer(Config{}, newStore()).cfg.ReplyDelay; d != 0 {
		t.Fatalf("the default reply delay is %s, want none", d)
	}
}
