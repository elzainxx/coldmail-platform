package main

import (
	"bufio"
	"errors"
	"fmt"
	"log"
	"net"
	"regexp"
	"strings"
	"sync"
	"time"
)

// The SMTP side of the fixture: the recipient's mail host, as the verifier node's gem sees it. For ONE verification the node makes two validations (the target, then
// the catch-all probe at nx-<hex>@ the same domain), and each is a plain TCP connect-and-close (the gem's port check), then a Net::SMTP session: banner, EHLO,
// MAIL FROM, RCPT TO, and QUIT unless an error ended the session (after a 4xx/5xx at RCPT or a timeout the client closes without QUIT). So this server tolerates a
// connection that opens and closes with no bytes, answers EHLO with a single plain 250 line (no STARTTLS, no AUTH), and decides ONLY at RCPT TO, per (domain,
// local part), from the rules in the state: a reply line, a stall (no answer, so the client's response timeout fires), or an abrupt close.

var (
	mailFromRe = regexp.MustCompile(`(?i)^FROM:\s*<([^<>\s]*)>(?:\s+.*)?$`)
	rcptToRe   = regexp.MustCompile(`(?i)^TO:\s*<([^<>\s]*)>(?:\s+.*)?$`)
)

const (
	smtpIdle      = 30 * time.Second
	smtpWrite     = 10 * time.Second
	smtpMaxLine   = 4096
	smtpMaxRcpts  = 100
	unknownDomain = "550 5.1.2 <{addr}> Domain not served by this fixture"
	noRuleReply   = "550 5.1.1 <{addr}> User unknown"
)

type smtpServer struct {
	st   *state
	host string
	logf func(format string, args ...any)

	ln    net.Listener
	quit  chan struct{}
	once  sync.Once
	wg    sync.WaitGroup
	mu    sync.Mutex
	conns map[net.Conn]struct{}
}

func newSMTPServer(st *state, ln net.Listener, hostname string, logf func(string, ...any)) *smtpServer {
	if hostname == "" {
		hostname = "mx.fixture.test"
	}
	if logf == nil {
		logf = log.Printf
	}
	return &smtpServer{st: st, ln: ln, host: hostname, logf: logf, quit: make(chan struct{}), conns: map[net.Conn]struct{}{}}
}

func (s *smtpServer) serve() {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			c, err := s.ln.Accept()
			if err != nil {
				select {
				case <-s.quit:
					return
				default:
				}
				if errors.Is(err, net.ErrClosed) {
					return
				}
				time.Sleep(50 * time.Millisecond)
				continue
			}
			s.mu.Lock()
			select {
			case <-s.quit:
				s.mu.Unlock()
				_ = c.Close()
				return
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
	}()
}

func (s *smtpServer) close() {
	s.once.Do(func() { close(s.quit) })
	_ = s.ln.Close()
	s.mu.Lock()
	for c := range s.conns {
		_ = c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

type smtpConn struct {
	s    *smtpServer
	c    net.Conn
	br   *bufio.Reader
	addr string // the client's address, without the port
}

func (sc *smtpConn) reply(format string, args ...any) bool {
	_ = sc.c.SetWriteDeadline(time.Now().Add(smtpWrite))
	_, err := fmt.Fprintf(sc.c, format+"\r\n", args...)
	return err == nil
}

func (s *smtpServer) serveConn(c net.Conn) {
	sc := &smtpConn{s: s, c: c, br: bufio.NewReaderSize(c, smtpMaxLine+16), addr: hostOf(c.RemoteAddr().String())}
	if !sc.reply("220 %s ESMTP fixture", s.host) {
		s.st.portCheck()
		return
	}
	session, helo, mailFrom, rcpts, haveMail := 0, "", "", 0, false
	ended := "eof"
	defer func() {
		if session == 0 {
			s.st.portCheck()
			return
		}
		s.logf("smtp: session=%d from=%s helo=%q mail_from=%q rcpts=%d ended=%s", session, sc.addr, helo, mailFrom, rcpts, ended)
	}()
	for {
		_ = c.SetReadDeadline(time.Now().Add(smtpIdle))
		line, err := sc.br.ReadString('\n')
		if err != nil {
			return
		}
		if len(line) > smtpMaxLine {
			sc.reply("500 5.5.2 line too long")
			continue
		}
		if session == 0 {
			session = s.st.newSession()
		}
		line = strings.TrimRight(line, "\r\n")
		cmd, arg := line, ""
		if i := strings.IndexAny(line, " \t"); i >= 0 {
			cmd, arg = line[:i], strings.TrimSpace(line[i+1:])
		}
		switch strings.ToUpper(cmd) {
		case "EHLO", "HELO":
			helo, haveMail, rcpts = arg, false, 0
			if !sc.reply("250 %s", s.host) {
				return
			}
		case "MAIL":
			m := mailFromRe.FindStringSubmatch(arg)
			if m == nil {
				sc.reply("501 5.5.4 syntax: MAIL FROM:<address>")
				continue
			}
			mailFrom, haveMail, rcpts = m[1], true, 0
			if !sc.reply("250 2.1.0 OK") {
				return
			}
		case "RCPT":
			if !haveMail {
				sc.reply("503 5.5.1 need MAIL before RCPT")
				continue
			}
			m := rcptToRe.FindStringSubmatch(arg)
			if m == nil || !strings.Contains(m[1], "@") || strings.HasPrefix(m[1], "@") || strings.HasSuffix(m[1], "@") {
				sc.reply("501 5.1.3 syntax: RCPT TO:<local@domain>")
				continue
			}
			if rcpts >= smtpMaxRcpts {
				sc.reply("452 4.5.3 too many recipients")
				continue
			}
			rcpts++
			at := strings.LastIndexByte(m[1], '@')
			local, domain := m[1][:at], strings.ToLower(m[1][at+1:])
			rule, matched := s.decide(domain, local)
			text := strings.ReplaceAll(rule.Reply, "{addr}", m[1])
			s.st.logSMTP(smtpEntry{Session: session, Remote: sc.addr, Helo: helo, MailFrom: mailFrom, Rcpt: m[1], Domain: domain, Rule: matched, Action: rule.action(), Reply: replyText(rule, text)})
			s.logf("smtp: session=%d from=%s rcpt=%s rule=%q action=%s reply=%q", session, sc.addr, m[1], matched, rule.action(), replyText(rule, text))
			switch rule.action() {
			case "drop":
				ended = "dropped at RCPT"
				return
			case "stall":
				ms := rule.StallMs
				if ms == 0 {
					ms = defaultStallMs
				}
				ended = "stalled at RCPT"
				t := time.NewTimer(time.Duration(ms) * time.Millisecond)
				select {
				case <-t.C:
				case <-s.quit:
					t.Stop()
				}
				return
			}
			if !sc.reply("%s", text) {
				return
			}
		case "RSET":
			haveMail, rcpts = false, 0
			if !sc.reply("250 2.0.0 OK") {
				return
			}
		case "NOOP":
			if !sc.reply("250 2.0.0 OK") {
				return
			}
		case "VRFY":
			if !sc.reply("252 2.5.2 cannot verify, but will accept") {
				return
			}
		case "QUIT":
			ended = "quit"
			sc.reply("221 2.0.0 bye")
			return
		case "DATA":
			sc.reply("503 5.5.1 this fixture takes no message")
		case "STARTTLS", "AUTH":
			sc.reply("502 5.5.1 %s is not offered", strings.ToUpper(cmd))
		default:
			sc.reply("500 5.5.2 command not recognized")
		}
	}
}

// decide picks the rule for one recipient: the first rule of the domain whose glob fits the local part. A domain the fixture does not serve, or a domain whose rules
// all miss, gets a fixed 550 so that a recipe that was not written can never read as an acceptance.
func (s *smtpServer) decide(domain, local string) (smtpRule, string) {
	rules, ok := s.st.rulesFor(domain)
	if !ok {
		return smtpRule{Match: "(domain not served)", Reply: unknownDomain}, "(domain not served)"
	}
	for _, r := range rules {
		if globMatch(r.Match, local) {
			return r, r.Match
		}
	}
	return smtpRule{Match: "(no rule)", Reply: noRuleReply}, "(no rule)"
}

func replyText(r smtpRule, text string) string {
	switch r.action() {
	case "stall":
		return "(no reply: stalled)"
	case "drop":
		return "(no reply: connection closed)"
	}
	return text
}
