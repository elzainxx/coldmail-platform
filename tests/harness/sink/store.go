package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"sort"
	"strings"
	"sync"
	"time"
)

// The state of the sink: what it was told to do (Mode), what it accepted (messages), what it said to each recipient (rcpt counters) and which
// sessions it is holding open right now (held). Everything is behind one mutex: the sink serves a handful of connections and an HTTP client that
// polls, so there is nothing to gain from finer locks and a lot to lose from a subtle one.

const (
	defaultRejectCode = 550
	maxDelayMs        = 120000 // two minutes: far above anything the harness needs, far below "forever"
	maxListEntries    = 5000
	maxAddrLen        = 320
	maxStoredMessages = 100000   // past it the sink refuses (452) instead of silently dropping: a lost message would read as an exactly-once pass
	maxStoredBody     = 1 << 20  // the body kept per message (tests read it; the API never serves it)
	maxHeaderBytes    = 64 << 10 // headers are parsed from this much of the message
)

// Mode is what the sink does right now. A POST to /api/mode REPLACES the whole mode: a field that is absent takes its default, so there is no hidden
// state and "what is the sink doing" is always the last body posted.
//
//	reject_rcpt      addresses answered at RCPT TO with reject_code and "no such user" (a hard reject: module 3 reports the item as bounced)
//	reject_code      the reply code of a reject: 400..599, default 550
//	defer_rcpt       addresses answered at RCPT TO with 451 4.7.1 try later (a temporary failure: module 3 requeues the item)
//	data_delay_ms    how long the final reply to a message is held back after the end-of-data (the connection stays open and the send is in flight)
//	delay_recipients when not empty, only messages to one of these addresses are delayed
//	fail_auth        every AUTH is answered 535
//
// An address in both reject_rcpt and defer_rcpt is rejected (the reject wins). Addresses are compared lower-cased.
type Mode struct {
	RejectRcpt      []string `json:"reject_rcpt"`
	RejectCode      int      `json:"reject_code"`
	DeferRcpt       []string `json:"defer_rcpt"`
	DataDelayMs     int      `json:"data_delay_ms"`
	DelayRecipients []string `json:"delay_recipients"`
	FailAuth        bool     `json:"fail_auth"`
}

func defaultMode() Mode {
	return Mode{RejectRcpt: []string{}, RejectCode: defaultRejectCode, DeferRcpt: []string{}, DelayRecipients: []string{}}
}

// normalize validates the mode and puts it in canonical form (lower-cased, de-duplicated lists; no nil slices). It returns the first problem found.
func (m *Mode) normalize() error {
	if m.RejectCode < 400 || m.RejectCode > 599 {
		return fmt.Errorf("reject_code must be a 4xx or 5xx reply code (400..599), got %d", m.RejectCode)
	}
	if m.DataDelayMs < 0 || m.DataDelayMs > maxDelayMs {
		return fmt.Errorf("data_delay_ms must be between 0 and %d, got %d", maxDelayMs, m.DataDelayMs)
	}
	var err error
	if m.RejectRcpt, err = cleanList("reject_rcpt", m.RejectRcpt); err != nil {
		return err
	}
	if m.DeferRcpt, err = cleanList("defer_rcpt", m.DeferRcpt); err != nil {
		return err
	}
	if m.DelayRecipients, err = cleanList("delay_recipients", m.DelayRecipients); err != nil {
		return err
	}
	return nil
}

func cleanList(name string, in []string) ([]string, error) {
	if len(in) > maxListEntries {
		return nil, fmt.Errorf("%s holds %d entries, the limit is %d", name, len(in), maxListEntries)
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, a := range in {
		a = strings.ToLower(strings.TrimSpace(a))
		switch {
		case a == "":
			return nil, fmt.Errorf("%s holds an empty address", name)
		case len(a) > maxAddrLen:
			return nil, fmt.Errorf("%s holds an address longer than %d bytes", name, maxAddrLen)
		case strings.ContainsAny(a, " \t\r\n<>,;\"") || strings.Contains(a, "\x00"):
			return nil, fmt.Errorf("%s holds %q: an address has no spaces, angle brackets, commas or quotes (write it as addr@example.test)", name, a)
		}
		if !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	return out, nil
}

// activeMode is a Mode with its lists as sets. It is built once per POST and never changed afterwards, so sessions may share it without a lock.
type activeMode struct {
	Mode
	reject   map[string]bool
	deferred map[string]bool
	delay    map[string]bool
}

func newActiveMode(m Mode) *activeMode {
	set := func(list []string) map[string]bool {
		s := make(map[string]bool, len(list))
		for _, a := range list {
			s[a] = true
		}
		return s
	}
	return &activeMode{Mode: m, reject: set(m.RejectRcpt), deferred: set(m.DeferRcpt), delay: set(m.DelayRecipients)}
}

// delayFor is how long a message to rcpts is held after the end-of-data (0 = not at all).
func (a *activeMode) delayFor(rcpts []string) time.Duration {
	if a.DataDelayMs <= 0 {
		return 0
	}
	if len(a.delay) > 0 {
		hit := false
		for _, r := range rcpts {
			if a.delay[r] {
				hit = true
				break
			}
		}
		if !hit {
			return 0
		}
	}
	return time.Duration(a.DataDelayMs) * time.Millisecond
}

// Message is one accepted message: accepted means the sink sent the final 250 (a message whose reply never went out because the connection died, or the
// sink was stopped, during a hold is not recorded).
type Message struct {
	Seq          int64     `json:"seq"`
	At           time.Time `json:"at"`
	To           []string  `json:"to"`
	From         string    `json:"from"`
	EnvelopeFrom string    `json:"envelope_from"`
	Subject      string    `json:"subject"`
	MessageID    string    `json:"message_id"`
	Size         int       `json:"size"`
	HeldMs       int64     `json:"held_ms"`

	body []byte // never served; the unit tests read it (dot-stuffing)
}

// RcptCount is what the sink answered to one recipient address over RCPT TO since the last reset.
type RcptCount struct {
	Accepted int `json:"accepted"`
	Rejected int `json:"rejected"`
	Deferred int `json:"deferred"`
}

type hold struct {
	to      []string
	started time.Time
	until   time.Time
}

// HeldRow is one session held in a delayed DATA.
type HeldRow struct {
	To          []string `json:"to"`
	AgeMs       int64    `json:"age_ms"`
	RemainingMs int64    `json:"remaining_ms"`
}

// InflightView is GET /api/inflight: how many sessions are held right now, the most there have been at once since the last reset, how many have been held
// in all, and which.
type InflightView struct {
	Inflight  int       `json:"inflight"`
	Peak      int       `json:"peak"`
	HeldTotal int       `json:"held_total"`
	Held      []HeldRow `json:"held"`
}

// Store is the sink's whole state.
type Store struct {
	mu        sync.Mutex
	mode      *activeMode
	messages  []Message
	seq       int64
	rcpt      map[string]*RcptCount
	held      map[int64]*hold
	holdSeq   int64
	peak      int
	heldTotal int
	idPrefix  string
	idSeq     int64 // never reset: the "queued as" ids must stay unique across resets (module 3 stores them under a UNIQUE constraint)
	now       func() time.Time
}

func newStore() *Store {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("sink: no randomness: " + err.Error())
	}
	return &Store{
		mode:     newActiveMode(defaultMode()),
		rcpt:     map[string]*RcptCount{},
		held:     map[int64]*hold{},
		idPrefix: "S" + strings.ToUpper(hex.EncodeToString(b[:])),
		now:      time.Now,
	}
}

func (s *Store) currentMode() *activeMode {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mode
}

// Mode returns a copy of the mode in force.
func (s *Store) Mode() Mode {
	m := s.currentMode().Mode
	m.RejectRcpt = append([]string{}, m.RejectRcpt...)
	m.DeferRcpt = append([]string{}, m.DeferRcpt...)
	m.DelayRecipients = append([]string{}, m.DelayRecipients...)
	return m
}

// SetMode installs m (already normalised) and returns a copy of it.
func (s *Store) SetMode(m Mode) Mode {
	am := newActiveMode(m)
	s.mu.Lock()
	s.mode = am
	s.mu.Unlock()
	return s.Mode()
}

// Reset forgets every message, every recipient counter, the peak and the held total, and puts the default mode back. Sessions that are being held are NOT
// released (they finish on their own clock and are recorded after the reset); the peak restarts from how many there are now. It returns how many
// messages were forgotten.
func (s *Store) Reset() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.messages)
	s.messages = nil
	s.seq = 0
	s.rcpt = map[string]*RcptCount{}
	s.heldTotal = 0
	s.peak = len(s.held)
	s.mode = newActiveMode(defaultMode())
	return n
}

// Raw returns a copy of the raw bytes of the accepted message with that seq (false when there is none).
func (s *Store) Raw(seq int64) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.messages {
		if m.Seq == seq {
			return append([]byte(nil), m.body...), true
		}
	}
	return nil, false
}

// Messages returns a copy of every accepted message, oldest first.
func (s *Store) Messages() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Message, len(s.messages))
	copy(out, s.messages)
	return out
}

func (s *Store) countRcpt(addr string, kind string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.rcpt[addr]
	if c == nil {
		c = &RcptCount{}
		s.rcpt[addr] = c
	}
	switch kind {
	case "accepted":
		c.Accepted++
	case "rejected":
		c.Rejected++
	case "deferred":
		c.Deferred++
	}
}

// Rcpt returns a copy of the per-recipient counters.
func (s *Store) Rcpt() map[string]RcptCount {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]RcptCount, len(s.rcpt))
	for a, c := range s.rcpt {
		out[a] = *c
	}
	return out
}

// beginHold registers a session held in a delayed DATA and returns its id; endHold releases it.
func (s *Store) beginHold(to []string, d time.Duration) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.holdSeq++
	now := s.now()
	s.held[s.holdSeq] = &hold{to: append([]string{}, to...), started: now, until: now.Add(d)}
	s.heldTotal++
	if len(s.held) > s.peak {
		s.peak = len(s.held)
	}
	return s.holdSeq
}

func (s *Store) endHold(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.held, id)
}

// Inflight returns the sessions held in a delayed DATA right now, oldest first.
func (s *Store) Inflight() InflightView {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	rows := make([]*hold, 0, len(s.held))
	for _, h := range s.held {
		rows = append(rows, h)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].started.Before(rows[j].started) })
	v := InflightView{Inflight: len(s.held), Peak: s.peak, HeldTotal: s.heldTotal, Held: make([]HeldRow, 0, len(rows))}
	for _, h := range rows {
		rem := h.until.Sub(now).Milliseconds()
		if rem < 0 {
			rem = 0
		}
		v.Held = append(v.Held, HeldRow{To: append([]string{}, h.to...), AgeMs: now.Sub(h.started).Milliseconds(), RemainingMs: rem})
	}
	return v
}

var errStoreFull = errors.New("the sink holds the maximum number of messages")

// record stores an accepted message and returns the id the final reply names ("queued as <id>"): unique for the life of the process.
func (s *Store) record(m Message) (Message, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.messages) >= maxStoredMessages {
		return Message{}, "", errStoreFull
	}
	s.seq++
	s.idSeq++
	m.Seq = s.seq
	m.At = s.now().UTC()
	s.messages = append(s.messages, m)
	return m, fmt.Sprintf("%s%08d", s.idPrefix, s.idSeq), nil
}

// ---- reading what a message says about itself ----

// parseHeaders reads the From, Subject and Message-ID of a message (the first of each; folded lines are unfolded; an RFC 2047 subject is decoded). It never
// fails: a header that is not there is the empty string.
func parseHeaders(raw []byte) (from, subject, messageID string) {
	if len(raw) > maxHeaderBytes {
		raw = raw[:maxHeaderBytes]
	}
	text := string(raw)
	end := len(text)
	if i := strings.Index(text, "\r\n\r\n"); i >= 0 {
		end = i
	}
	if i := strings.Index(text, "\n\n"); i >= 0 && i < end {
		end = i
	}
	text = text[:end]
	text = strings.ReplaceAll(text, "\r\n", "\n")
	var lines []string
	for _, l := range strings.Split(text, "\n") {
		if (strings.HasPrefix(l, " ") || strings.HasPrefix(l, "\t")) && len(lines) > 0 {
			lines[len(lines)-1] += " " + strings.TrimSpace(l)
			continue
		}
		lines = append(lines, l)
	}
	dec := new(mime.WordDecoder)
	for _, l := range lines {
		name, value, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "from":
			if from == "" {
				from = value
			}
		case "subject":
			if subject == "" {
				if d, err := dec.DecodeHeader(value); err == nil {
					value = d
				}
				subject = value
			}
		case "message-id":
			if messageID == "" {
				messageID = value
			}
		}
	}
	return from, subject, messageID
}
