package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The SMTP side of the sink: what module 3's worker speaks (coldmail-send-byo services/worker/src/smtp.js): banner, EHLO, STARTTLS, EHLO again, AUTH LOGIN (or PLAIN),
// MAIL FROM, RCPT TO, DATA, QUIT. It answers like a real submission server so that the worker's own classification applies unchanged: a 550 at RCPT is a hard
// bounce, a 4xx at RCPT is a temporary failure, a final 250 names the message ("queued as ...").

// Config is how the SMTP server behaves; the modes (what to reject, what to delay) are in the Store.
type Config struct {
	Hostname        string
	TLS             *tls.Config // nil: STARTTLS is not offered
	RequireTLS      bool        // AUTH and MAIL are refused (530) until STARTTLS has been done; only meaningful with TLS
	RequireAuth     bool        // MAIL is refused (530) until AUTH succeeded
	MaxMessageBytes int
	IdleTimeout     time.Duration // how long a connection may be silent between commands
	// ReplyDelay is a pause before EVERY reply (0 = none). It is NOT politeness: module 3's worker (services/worker/src/smtp.js, SmtpConn) reads a reply only if its waiter is already registered
	// (#pushLine drops a reply that finds none) and registers it after the write callback of the command it answers; a sink that answers within a fraction of a millisecond can win that race
	// (measured against the real worker code over loopback: about one session in ten stalled until the worker's operation timeout, none in 160 with 2 ms). The harness runs the sink with 8 ms.
	ReplyDelay time.Duration
	Logf       func(format string, args ...any)
}

const (
	maxCommandLine = 4096
	maxRecipients  = 100
	writeTimeout   = 30 * time.Second
)

var (
	mailFromRe = regexp.MustCompile(`(?i)^FROM:\s*<([^<>\s]*)>(?:\s+(.*))?$`)
	rcptToRe   = regexp.MustCompile(`(?i)^TO:\s*<([^<>\s]*)>(?:\s+.*)?$`)
	sizeParams = regexp.MustCompile(`(?i)(?:^|\s)SIZE=(\d+)(?:\s|$)`)
)

// Server serves SMTP on a listener and records into a Store.
type Server struct {
	cfg   Config
	st    *Store
	quit  chan struct{}
	once  sync.Once
	wg    sync.WaitGroup
	ready atomic.Bool

	mu    sync.Mutex
	conns map[net.Conn]struct{}
}

func newServer(cfg Config, st *Store) *Server {
	if cfg.Hostname == "" {
		cfg.Hostname = "smtp-ctl"
	}
	if cfg.MaxMessageBytes <= 0 {
		cfg.MaxMessageBytes = 10 << 20
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 2 * time.Minute
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	return &Server{cfg: cfg, st: st, quit: make(chan struct{}), conns: map[net.Conn]struct{}{}}
}

// Ready is true once Serve is accepting connections.
func (s *Server) Ready() bool { return s.ready.Load() }

// Serve accepts connections until the listener is closed or Shutdown is called.
func (s *Server) Serve(ln net.Listener) error {
	s.ready.Store(true)
	defer s.ready.Store(false)
	for {
		c, err := ln.Accept()
		if err != nil {
			select {
			case <-s.quit:
				return nil
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			s.cfg.Logf("smtp: accept: %v", err)
			select {
			case <-s.quit:
				return nil
			case <-time.After(50 * time.Millisecond):
			}
			continue
		}
		s.mu.Lock()
		select {
		case <-s.quit: // shut down between Accept and here
			s.mu.Unlock()
			_ = c.Close()
			return nil
		default:
		}
		s.conns[c] = struct{}{}
		s.wg.Add(1)
		s.mu.Unlock()
		go func() {
			defer s.wg.Done()
			defer func() {
				s.mu.Lock()
				delete(s.conns, c)
				s.mu.Unlock()
				_ = c.Close()
			}()
			s.serveConn(c)
		}()
	}
}

// Shutdown stops accepting, releases every held session (they are answered 421 and not recorded) and closes every connection.
func (s *Server) Shutdown() {
	s.once.Do(func() { close(s.quit) })
	s.mu.Lock()
	for c := range s.conns {
		_ = c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

type session struct {
	srv   *Server
	raw   net.Conn // the TCP connection (what Shutdown closes)
	conn  net.Conn // raw, or the TLS connection over it
	br    *bufio.Reader
	tls   bool
	hello bool
	authd bool

	haveFrom bool
	from     string
	tried    int // RCPT commands received in this transaction
	rcpts    []string
}

var errLineTooLong = errors.New("line too long")

func (s *Server) serveConn(c net.Conn) {
	ss := &session{srv: s, raw: c, conn: c, br: bufio.NewReaderSize(c, 4096)}
	if ss.reply(220, fmt.Sprintf("%s ESMTP controllable sink ready", s.cfg.Hostname)) != nil {
		return
	}
	for {
		line, err := ss.readLine()
		if errors.Is(err, errLineTooLong) {
			if ss.reply(500, "5.5.2 line too long") != nil {
				return
			}
			continue
		}
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				_ = ss.reply(421, "4.4.2 idle timeout, closing")
			}
			return
		}
		if !ss.command(line) {
			return
		}
	}
}

func (ss *session) reply(code int, lines ...string) error {
	if d := ss.srv.cfg.ReplyDelay; d > 0 {
		time.Sleep(d)
	}
	var b bytes.Buffer
	for i, l := range lines {
		sep := "-"
		if i == len(lines)-1 {
			sep = " "
		}
		fmt.Fprintf(&b, "%d%s%s\r\n", code, sep, l)
	}
	_ = ss.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	_, err := ss.conn.Write(b.Bytes())
	return err
}

// readLine reads one command line (CRLF or LF ended), without the line ending. A line longer than maxCommandLine is discarded to its end and reported as errLineTooLong.
func (ss *session) readLine() (string, error) {
	_ = ss.conn.SetReadDeadline(time.Now().Add(ss.srv.cfg.IdleTimeout))
	tooLong := false
	var acc []byte
	for {
		chunk, err := ss.br.ReadSlice('\n')
		if len(acc)+len(chunk) > maxCommandLine {
			tooLong = true
		}
		if !tooLong {
			acc = append(acc, chunk...)
		}
		switch {
		case err == nil:
			if tooLong {
				return "", errLineTooLong
			}
			return strings.TrimRight(string(acc), "\r\n"), nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		default:
			return "", err
		}
	}
}

func (ss *session) resetTxn() {
	ss.haveFrom, ss.from, ss.tried, ss.rcpts = false, "", 0, nil
}

// command handles one command line and reports whether the session goes on.
func (ss *session) command(line string) bool {
	verb, arg, _ := strings.Cut(strings.TrimSpace(line), " ")
	arg = strings.TrimSpace(arg)
	switch strings.ToUpper(verb) {
	case "EHLO":
		return ss.cmdHello(arg, true)
	case "HELO":
		return ss.cmdHello(arg, false)
	case "STARTTLS":
		return ss.cmdStartTLS(arg)
	case "AUTH":
		return ss.cmdAuth(arg)
	case "MAIL":
		return ss.cmdMail(arg)
	case "RCPT":
		return ss.cmdRcpt(arg)
	case "DATA":
		return ss.cmdData()
	case "RSET":
		ss.resetTxn()
		return ss.reply(250, "2.0.0 OK") == nil
	case "NOOP":
		return ss.reply(250, "2.0.0 OK") == nil
	case "VRFY":
		return ss.reply(252, "2.5.2 cannot VRFY a user, but will accept the message") == nil
	case "HELP":
		return ss.reply(214, "2.0.0 controllable SMTP sink of the platform harness") == nil
	case "QUIT":
		_ = ss.reply(221, "2.0.0 Bye")
		return false
	case "":
		return ss.reply(500, "5.5.2 empty command") == nil
	default:
		return ss.reply(502, "5.5.2 command not recognized") == nil
	}
}

func (ss *session) cmdHello(arg string, extended bool) bool {
	if arg == "" {
		return ss.reply(501, "5.5.4 EHLO/HELO needs a domain") == nil
	}
	ss.hello = true
	ss.resetTxn()
	cfg := ss.srv.cfg
	if !extended {
		return ss.reply(250, cfg.Hostname) == nil
	}
	lines := []string{fmt.Sprintf("%s greets %s", cfg.Hostname, arg), "ENHANCEDSTATUSCODES", "8BITMIME", fmt.Sprintf("SIZE %d", cfg.MaxMessageBytes)}
	// AUTH is advertised only where it may be used: after STARTTLS when TLS is required (a real submission server never offers a credential exchange in the clear)
	if ss.tls || cfg.TLS == nil || !cfg.RequireTLS {
		lines = append(lines, "AUTH PLAIN LOGIN")
	}
	if cfg.TLS != nil && !ss.tls {
		lines = append(lines, "STARTTLS")
	}
	return ss.reply(250, lines...) == nil
}

func (ss *session) cmdStartTLS(arg string) bool {
	cfg := ss.srv.cfg
	switch {
	case cfg.TLS == nil:
		return ss.reply(454, "4.7.0 TLS not available") == nil
	case ss.tls:
		return ss.reply(503, "5.5.1 TLS is already active") == nil
	case arg != "":
		return ss.reply(501, "5.5.4 STARTTLS takes no parameters") == nil
	case !ss.hello:
		return ss.reply(503, "5.5.1 send EHLO first") == nil
	}
	// RFC 3207: whatever the client pipelined behind STARTTLS must not be executed under the new security state (the plaintext injection attack): refuse the upgrade.
	if ss.br.Buffered() > 0 {
		_ = ss.reply(501, "5.5.4 data after STARTTLS")
		return false
	}
	if ss.reply(220, "2.0.0 Ready to start TLS") != nil {
		return false
	}
	tc := tls.Server(ss.raw, cfg.TLS)
	_ = ss.raw.SetDeadline(time.Now().Add(writeTimeout))
	if err := tc.Handshake(); err != nil {
		cfg.Logf("smtp: TLS handshake failed: %v", err)
		return false
	}
	_ = ss.raw.SetDeadline(time.Time{})
	ss.conn, ss.br, ss.tls = tc, bufio.NewReaderSize(tc, 4096), true
	// RFC 3207 section 4.2: the session starts over; nothing said before the handshake counts
	ss.hello, ss.authd = false, false
	ss.resetTxn()
	return true
}

// cmdAuth accepts ANY credential (PLAIN or LOGIN) unless the mode says fail_auth. The credential itself is never read into anything: it is decoded only to prove it is base64.
func (ss *session) cmdAuth(arg string) bool {
	cfg := ss.srv.cfg
	switch {
	case !ss.hello:
		return ss.reply(503, "5.5.1 send EHLO first") == nil
	case cfg.TLS != nil && cfg.RequireTLS && !ss.tls:
		return ss.reply(530, "5.7.0 Must issue a STARTTLS command first") == nil
	case ss.authd:
		return ss.reply(503, "5.5.1 already authenticated") == nil
	}
	mech, initial, _ := strings.Cut(arg, " ")
	initial = strings.TrimSpace(initial)
	switch strings.ToUpper(mech) {
	case "PLAIN":
		if initial == "" {
			if ss.reply(334, "") != nil {
				return false
			}
			line, err := ss.readLine()
			if err != nil {
				return false
			}
			initial = strings.TrimSpace(line)
		}
		if initial == "*" {
			return ss.reply(501, "5.0.0 authentication cancelled") == nil
		}
		if _, err := base64.StdEncoding.DecodeString(initial); err != nil {
			return ss.reply(501, "5.5.2 cannot decode the authentication response") == nil
		}
	case "LOGIN":
		// two challenges, "Username:" and "Password:"; an initial response stands for the answer to the first
		step := 0
		line := initial
		for step < 2 {
			if step > 0 || line == "" {
				challenge := "Username:"
				if step == 1 {
					challenge = "Password:"
				}
				if ss.reply(334, base64.StdEncoding.EncodeToString([]byte(challenge))) != nil {
					return false
				}
				l, err := ss.readLine()
				if err != nil {
					return false
				}
				line = strings.TrimSpace(l)
			}
			if line == "*" {
				return ss.reply(501, "5.0.0 authentication cancelled") == nil
			}
			if _, err := base64.StdEncoding.DecodeString(line); err != nil {
				return ss.reply(501, "5.5.2 cannot decode the authentication response") == nil
			}
			step++
		}
	default:
		return ss.reply(504, "5.5.4 unrecognized authentication type") == nil
	}
	if ss.srv.st.currentMode().FailAuth {
		ss.srv.cfg.Logf("smtp: AUTH refused (fail_auth)")
		return ss.reply(535, "5.7.8 Authentication credentials invalid") == nil
	}
	ss.authd = true
	return ss.reply(235, "2.7.0 Authentication successful") == nil
}

func (ss *session) cmdMail(arg string) bool {
	cfg := ss.srv.cfg
	switch {
	case !ss.hello:
		return ss.reply(503, "5.5.1 send EHLO first") == nil
	case cfg.TLS != nil && cfg.RequireTLS && !ss.tls:
		return ss.reply(530, "5.7.0 Must issue a STARTTLS command first") == nil
	case cfg.RequireAuth && !ss.authd:
		return ss.reply(530, "5.7.0 Authentication required") == nil
	case ss.haveFrom:
		return ss.reply(503, "5.5.1 nested MAIL command") == nil
	}
	m := mailFromRe.FindStringSubmatch(arg)
	if m == nil {
		return ss.reply(501, "5.5.4 syntax: MAIL FROM:<address>") == nil
	}
	if sz := sizeParams.FindStringSubmatch(m[2]); sz != nil {
		if n, err := strconv.ParseInt(sz[1], 10, 64); err != nil || n > int64(cfg.MaxMessageBytes) {
			return ss.reply(552, "5.3.4 message size exceeds the limit") == nil
		}
	}
	ss.haveFrom, ss.from, ss.tried, ss.rcpts = true, strings.ToLower(m[1]), 0, nil
	return ss.reply(250, "2.1.0 OK") == nil
}

func (ss *session) cmdRcpt(arg string) bool {
	if !ss.haveFrom {
		return ss.reply(503, "5.5.1 need MAIL command first") == nil
	}
	m := rcptToRe.FindStringSubmatch(arg)
	if m == nil || m[1] == "" {
		return ss.reply(501, "5.1.3 syntax: RCPT TO:<address>") == nil
	}
	addr := strings.ToLower(m[1])
	if len(ss.rcpts) >= maxRecipients {
		return ss.reply(452, "4.5.3 too many recipients") == nil
	}
	ss.tried++
	mode := ss.srv.st.currentMode()
	switch {
	case mode.reject[addr]:
		ss.srv.st.countRcpt(addr, "rejected")
		ss.srv.cfg.Logf("smtp: RCPT %s rejected with %d", addr, mode.RejectCode)
		return ss.reply(mode.RejectCode, fmt.Sprintf("%d.1.1 no such user", mode.RejectCode/100)) == nil
	case mode.deferred[addr]:
		ss.srv.st.countRcpt(addr, "deferred")
		ss.srv.cfg.Logf("smtp: RCPT %s deferred with 451", addr)
		return ss.reply(451, "4.7.1 try later") == nil
	}
	ss.srv.st.countRcpt(addr, "accepted")
	ss.rcpts = append(ss.rcpts, addr)
	return ss.reply(250, "2.1.5 OK") == nil
}

// cmdData reads the message, holds the final reply when the mode says so (the session is then "in flight": the client has sent everything and waits), records the message and answers 250.
func (ss *session) cmdData() bool {
	switch {
	case !ss.haveFrom:
		return ss.reply(503, "5.5.1 need MAIL command first") == nil
	case len(ss.rcpts) == 0 && ss.tried == 0:
		return ss.reply(503, "5.5.1 need RCPT command first") == nil
	case len(ss.rcpts) == 0:
		ss.resetTxn()
		return ss.reply(554, "5.5.1 no valid recipients") == nil
	}
	if ss.reply(354, "End data with <CR><LF>.<CR><LF>") != nil {
		return false
	}
	body, tooBig, err := ss.readData()
	if err != nil {
		return false
	}
	rcpts, from := ss.rcpts, ss.from
	ss.resetTxn()
	if tooBig {
		return ss.reply(552, "5.3.4 message too big") == nil
	}

	var held time.Duration
	if d := ss.srv.st.currentMode().delayFor(rcpts); d > 0 {
		id := ss.srv.st.beginHold(rcpts, d)
		ss.srv.cfg.Logf("smtp: holding the reply for %s (%v)", d, rcpts)
		t := time.NewTimer(d)
		start := time.Now()
		select {
		case <-t.C:
		case <-ss.srv.quit:
			t.Stop()
			ss.srv.st.endHold(id)
			_ = ss.reply(421, "4.3.2 service shutting down")
			return false
		}
		held = time.Since(start)
		ss.srv.st.endHold(id)
	}

	hdrFrom, subject, msgID := parseHeaders(body)
	stored := body
	if len(stored) > maxStoredBody {
		stored = stored[:maxStoredBody]
	}
	msg, queuedAs, err := ss.srv.st.record(Message{
		To: rcpts, From: hdrFrom, EnvelopeFrom: from, Subject: subject, MessageID: msgID, Size: len(body), HeldMs: held.Milliseconds(), body: stored,
	})
	if err != nil {
		return ss.reply(452, "4.3.1 insufficient storage") == nil
	}
	ss.srv.cfg.Logf("smtp: accepted message %d to %v", msg.Seq, rcpts)
	return ss.reply(250, "2.0.0 Ok: queued as "+queuedAs) == nil
}

// readData reads the message body up to the end-of-data line, undoing dot-stuffing. A message over MaxMessageBytes is read to its end and reported as tooBig (its bytes are dropped).
func (ss *session) readData() (body []byte, tooBig bool, err error) {
	_ = ss.conn.SetReadDeadline(time.Now().Add(ss.srv.cfg.IdleTimeout))
	limit := ss.srv.cfg.MaxMessageBytes
	var buf bytes.Buffer
	total := 0
	atStart := true
	for {
		chunk, rerr := ss.br.ReadSlice('\n')
		if rerr != nil && !errors.Is(rerr, bufio.ErrBufferFull) {
			if errors.Is(rerr, io.EOF) {
				rerr = io.ErrUnexpectedEOF
			}
			return nil, false, rerr
		}
		complete := rerr == nil
		if atStart {
			if complete && (string(chunk) == ".\r\n" || string(chunk) == ".\n") {
				break
			}
			if len(chunk) > 0 && chunk[0] == '.' {
				chunk = chunk[1:] // dot-unstuffing: a leading dot of a line was doubled by the sender
			}
		}
		atStart = complete
		total += len(chunk)
		if total > limit {
			tooBig = true
			continue
		}
		buf.Write(chunk)
		_ = ss.conn.SetReadDeadline(time.Now().Add(ss.srv.cfg.IdleTimeout))
	}
	if tooBig {
		return nil, true, nil
	}
	return buf.Bytes(), false, nil
}
