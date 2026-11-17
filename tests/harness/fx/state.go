package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The shared state of the fixture: what the DNS side answers, what the SMTP side says at RCPT, and the logs the control API serves. One process, one state, so a test
// that changes the behaviour of ONE domain through the control API changes it for DNS and SMTP clients at once and for nobody else.

// fixtureConfig is the file the harness mounts (tests/harness/m1node/fixture.json). Two placeholders are expanded from the environment before it is parsed:
// ${FX_SELF_IP} (the fixture's own address on the fixture network) and ${FX_NODE_IP} (the verifier node's); any other placeholder, or an unset one, is an error.
type fixtureConfig struct {
	TTL     int              `json:"ttl"`
	Zones   []string         `json:"zones"`
	Records []recordSpec     `json:"records"`
	Modes   []modeSpec       `json:"modes"`
	SMTP    []smtpDomainSpec `json:"smtp"`
}

type recordSpec struct {
	Name   string   `json:"name"`
	Type   string   `json:"type"`
	Values []string `json:"values"`
}

type modeSpec struct {
	Name string `json:"name"`
	Mode string `json:"mode"`
}

type smtpDomainSpec struct {
	Domain string     `json:"domain"`
	Rules  []smtpRule `json:"rules"`
}

// smtpRule is one line of a domain's behaviour at RCPT TO. The first rule whose Match fits the local part of the recipient decides.
type smtpRule struct {
	// Match is a glob over the local part, case-insensitive; the only wildcard is `*`.
	Match string `json:"match"`
	// Action is "reply" (the default: answer Reply), "stall" (say nothing for StallMs, then close: the client's response timeout fires first) or "drop" (close the
	// connection at once, no reply).
	Action  string `json:"action,omitempty"`
	Reply   string `json:"reply,omitempty"`
	StallMs int    `json:"stall_ms,omitempty"`
}

func (r smtpRule) action() string {
	if r.Action == "" {
		return "reply"
	}
	return r.Action
}

const (
	defaultStallMs = 6000
	maxStallMs     = 30000
	defaultTTL     = 1
	maxLogEntries  = 4000
)

var (
	envNames     = map[string]bool{"FX_SELF_IP": true, "FX_NODE_IP": true}
	placeholder  = regexp.MustCompile(`\$\{([A-Za-z0-9_]+)\}`)
	replyLineRe  = regexp.MustCompile(`^[245][0-9]{2}( .*)?$`)
	validModes   = map[string]bool{"normal": true, "servfail": true, "refused": true, "silent": true, "nxdomain": true}
	validRRTypes = map[string]bool{"A": true, "MX": true, "TXT": true, "PTR": true}
)

// expandConfig replaces every ${NAME} with the environment's value. A name outside the two the fixture defines, or an unset one, is an error: a config that
// silently kept a literal "${FX_NODE_IP}" would publish a nonsense address and the harness would fail far from the cause.
func expandConfig(raw []byte, getenv func(string) string) ([]byte, error) {
	var firstErr error
	out := placeholder.ReplaceAllFunc(raw, func(m []byte) []byte {
		name := string(placeholder.FindSubmatch(m)[1])
		if !envNames[name] {
			if firstErr == nil {
				firstErr = fmt.Errorf("the config uses the placeholder ${%s}, which is not one of FX_SELF_IP, FX_NODE_IP", name)
			}
			return m
		}
		v := strings.TrimSpace(getenv(name))
		if v == "" {
			if firstErr == nil {
				firstErr = fmt.Errorf("the config uses ${%s} but the environment variable is not set", name)
			}
			return m
		}
		return []byte(v)
	})
	return out, firstErr
}

// loadConfig reads and validates the fixture file. Unknown JSON keys are errors (a misspelt key must not read as "no change").
func loadConfig(path string, getenv func(string) string) (*fixtureConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read the fixture config: %w", err)
	}
	return parseConfig(raw, getenv)
}

func parseConfig(raw []byte, getenv func(string) string) (*fixtureConfig, error) {
	raw, err := expandConfig(raw, getenv)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var c fixtureConfig
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("the fixture config is not valid: %w", err)
	}
	if c.TTL < 0 || c.TTL > 3600 {
		return nil, errors.New("ttl must be 0..3600")
	}
	if c.TTL == 0 {
		c.TTL = defaultTTL
	}
	if len(c.Zones) == 0 {
		return nil, errors.New("the config names no zone")
	}
	for i, z := range c.Zones {
		z = normName(z)
		if z == "" {
			return nil, fmt.Errorf("zone %d is empty", i)
		}
		c.Zones[i] = z
	}
	for i, r := range c.Records {
		if err := validateRecord(r.Name, r.Type, r.Values); err != nil {
			return nil, fmt.Errorf("record %d (%s %s): %w", i, r.Name, r.Type, err)
		}
	}
	for i, m := range c.Modes {
		if normName(m.Name) == "" || !validModes[m.Mode] {
			return nil, fmt.Errorf("mode %d (%q %q): a name and one of normal, servfail, refused, silent, nxdomain are needed", i, m.Name, m.Mode)
		}
	}
	seen := map[string]bool{}
	for i, d := range c.SMTP {
		dom := normName(d.Domain)
		if dom == "" || seen[dom] {
			return nil, fmt.Errorf("smtp entry %d: the domain %q is empty or listed twice", i, d.Domain)
		}
		seen[dom] = true
		if err := validateRules(d.Rules); err != nil {
			return nil, fmt.Errorf("smtp domain %s: %w", dom, err)
		}
	}
	return &c, nil
}

func normName(s string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(s), "."))
}

func validateRecord(name, typ string, values []string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("a record needs a name")
	}
	if !validRRTypes[typ] {
		return fmt.Errorf("the type %q is not one of A, MX, TXT, PTR", typ)
	}
	if typ == "PTR" {
		if _, ok := reverseName(name); !ok {
			return errors.New("a PTR record is named by an IPv4 address (or its in-addr.arpa name)")
		}
	}
	for _, v := range values {
		switch typ {
		case "A":
			if ip := net.ParseIP(v); ip == nil || ip.To4() == nil {
				return fmt.Errorf("%q is not an IPv4 address", v)
			}
		case "MX":
			if _, _, err := parseMX(v); err != nil {
				return err
			}
		case "PTR":
			if _, err := encodeName(v); err != nil {
				return err
			}
		case "TXT":
			if len(v) > 1000 {
				return errors.New("a TXT value is longer than 1000 bytes")
			}
		}
	}
	return nil
}

func validateRules(rules []smtpRule) error {
	if len(rules) == 0 {
		return errors.New("at least one rule is needed")
	}
	for i, r := range rules {
		if strings.TrimSpace(r.Match) == "" {
			return fmt.Errorf("rule %d has no match", i)
		}
		switch r.action() {
		case "reply":
			if !replyLineRe.MatchString(r.Reply) {
				return fmt.Errorf("rule %d: the reply %q is not an SMTP reply line (a 2xx, 4xx or 5xx code, then optional text)", i, r.Reply)
			}
		case "stall":
			if r.StallMs < 0 || r.StallMs > maxStallMs {
				return fmt.Errorf("rule %d: stall_ms must be 0..%d", i, maxStallMs)
			}
		case "drop":
		default:
			return fmt.Errorf("rule %d: the action %q is not reply, stall or drop", i, r.Action)
		}
	}
	return nil
}

// reverseName turns an IPv4 address into its in-addr.arpa name; a name that is already one is returned normalised.
func reverseName(s string) (string, bool) {
	s = normName(s)
	if strings.HasSuffix(s, ".in-addr.arpa") {
		return s, true
	}
	ip := net.ParseIP(s)
	if ip == nil || ip.To4() == nil {
		return "", false
	}
	b := ip.To4()
	return fmt.Sprintf("%d.%d.%d.%d.in-addr.arpa", b[3], b[2], b[1], b[0]), true
}

func parseMX(v string) (uint16, string, error) {
	f := strings.Fields(v)
	if len(f) != 2 {
		return 0, "", fmt.Errorf("an MX value is \"<preference> <host>\", got %q", v)
	}
	p, err := strconv.ParseUint(f[0], 10, 16)
	if err != nil {
		return 0, "", fmt.Errorf("the MX preference %q is not a number 0..65535", f[0])
	}
	if _, err := encodeName(f[1]); err != nil {
		return 0, "", err
	}
	return uint16(p), f[1], nil
}

// ---- the state ---------------------------------------------------------------------------------------------------------------------------------------

type dnsEntry struct {
	Seq    int       `json:"seq"`
	At     time.Time `json:"at"`
	Remote string    `json:"remote"`
	Proto  string    `json:"proto"`
	Name   string    `json:"name"`
	Type   string    `json:"type"`
	Kind   string    `json:"kind"` // answer, nodata, nxdomain, servfail, refused, silent, forward, formerr, notimp
}

type smtpEntry struct {
	Seq      int       `json:"seq"`
	At       time.Time `json:"at"`
	Session  int       `json:"session"`
	Remote   string    `json:"remote"`
	Helo     string    `json:"helo"`
	MailFrom string    `json:"mail_from"`
	Rcpt     string    `json:"rcpt"`
	Domain   string    `json:"domain"`
	Rule     string    `json:"rule"`
	Action   string    `json:"action"`
	Reply    string    `json:"reply"`
}

type ipifyEntry struct {
	Seq      int       `json:"seq"`
	At       time.Time `json:"at"`
	Remote   string    `json:"remote"`
	Answered string    `json:"answered"`
}

type counters struct {
	DNSQueries   int `json:"dns_queries"`
	SMTPSessions int `json:"smtp_sessions"`
	SMTPRcpts    int `json:"smtp_rcpts"`
	PortChecks   int `json:"smtp_connections_without_commands"`
	IpifyAnswers int `json:"ipify_answers"`
}

type state struct {
	mu       sync.RWMutex
	orig     *fixtureConfig
	upstream string

	zones   []string
	ttl     uint32
	records map[string]map[string][]string // name -> type -> values
	modes   map[string]string
	smtp    map[string][]smtpRule

	dnsLog   []dnsEntry
	smtpLog  []smtpEntry
	ipifyLog []ipifyEntry
	seq      int
	sessions int
	cnt      counters
}

func newState(cfg *fixtureConfig, upstream string) *state {
	s := &state{orig: cfg, upstream: upstream}
	s.load()
	return s
}

// load (re)builds the mutable tables from the original config. Callers hold no lock (newState) or the write lock (reset).
func (s *state) load() {
	cfg := s.orig
	s.zones = append([]string(nil), cfg.Zones...)
	sort.Slice(s.zones, func(i, j int) bool { return len(s.zones[i]) > len(s.zones[j]) }) // the longest apex wins
	s.ttl = uint32(cfg.TTL)
	s.records = map[string]map[string][]string{}
	for _, r := range cfg.Records {
		s.putRecords(r.Name, r.Type, r.Values)
	}
	s.modes = map[string]string{}
	for _, m := range cfg.Modes {
		if m.Mode != "normal" {
			s.modes[normName(m.Name)] = m.Mode
		}
	}
	s.smtp = map[string][]smtpRule{}
	for _, d := range cfg.SMTP {
		s.smtp[normName(d.Domain)] = append([]smtpRule(nil), d.Rules...)
	}
}

func (s *state) putRecords(name, typ string, values []string) {
	n := normName(name)
	if typ == "PTR" {
		if rn, ok := reverseName(n); ok {
			n = rn
		}
	}
	if len(values) == 0 {
		delete(s.records[n], typ)
		if len(s.records[n]) == 0 {
			delete(s.records, n)
		}
		return
	}
	if s.records[n] == nil {
		s.records[n] = map[string][]string{}
	}
	s.records[n][typ] = append([]string(nil), values...)
}

// reset puts the startup tables back and forgets every log line and counter.
func (s *state) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	s.dnsLog, s.smtpLog, s.ipifyLog = nil, nil, nil
	s.cnt = counters{}
}

func (s *state) setMode(name, mode string) error {
	name = normName(name)
	if name == "" || !validModes[mode] {
		return errors.New("a name and a mode (normal, servfail, refused, silent, nxdomain) are needed")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if mode == "normal" {
		delete(s.modes, name)
	} else {
		s.modes[name] = mode
	}
	return nil
}

func (s *state) setRecords(name, typ string, values []string) error {
	if err := validateRecord(name, typ, values); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.putRecords(name, typ, values)
	return nil
}

func (s *state) setRules(domain string, rules []smtpRule) error {
	domain = normName(domain)
	if domain == "" {
		return errors.New("a domain is needed")
	}
	if err := validateRules(rules); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.smtp[domain] = append([]smtpRule(nil), rules...)
	return nil
}

func (s *state) rulesFor(domain string) ([]smtpRule, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.smtp[normName(domain)]
	return r, ok
}

func (s *state) logDNS(e dnsEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	e.Seq, e.At = s.seq, time.Now().UTC()
	s.cnt.DNSQueries++
	s.dnsLog = append(s.dnsLog, e)
	if len(s.dnsLog) > maxLogEntries {
		s.dnsLog = s.dnsLog[len(s.dnsLog)-maxLogEntries:]
	}
}

func (s *state) logSMTP(e smtpEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	e.Seq, e.At = s.seq, time.Now().UTC()
	s.cnt.SMTPRcpts++
	s.smtpLog = append(s.smtpLog, e)
	if len(s.smtpLog) > maxLogEntries {
		s.smtpLog = s.smtpLog[len(s.smtpLog)-maxLogEntries:]
	}
}

func (s *state) logIpify(e ipifyEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	e.Seq, e.At = s.seq, time.Now().UTC()
	s.cnt.IpifyAnswers++
	s.ipifyLog = append(s.ipifyLog, e)
}

func (s *state) newSession() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions++
	s.cnt.SMTPSessions++
	return s.sessions
}

func (s *state) portCheck() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cnt.PortChecks++
}

func (s *state) counts() counters {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cnt
}

// globMatch is the rule matcher: `*` matches any run of characters (including none), everything else matches itself, case-insensitively.
func globMatch(pattern, s string) bool {
	p, t := strings.ToLower(pattern), strings.ToLower(s)
	var rec func(pi, ti int) bool
	rec = func(pi, ti int) bool {
		for pi < len(p) {
			if p[pi] == '*' {
				for pi < len(p) && p[pi] == '*' {
					pi++
				}
				if pi == len(p) {
					return true
				}
				for k := ti; k <= len(t); k++ {
					if rec(pi, k) {
						return true
					}
				}
				return false
			}
			if ti >= len(t) || p[pi] != t[ti] {
				return false
			}
			pi++
			ti++
		}
		return ti == len(t)
	}
	return rec(0, 0)
}
