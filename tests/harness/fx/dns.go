package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
)

// The DNS side of the fixture: an authoritative server (UDP and TCP) with the standard library only, because the recipes need things a static zone file cannot do
// and a test must be able to change per name while the stack runs: a SERVFAIL, a REFUSED, SILENCE (no reply at all), NXDOMAIN with an SOA, NODATA with an SOA, and
// a forwarder for the names that belong to Docker (a dotless service name like `postgres`, which the control plane's own database host name is).
//
// What it speaks: one question per query (class IN or ANY), the types A, MX, TXT, PTR, SOA and ANY (AAAA, NS and every other type are answered NODATA when the
// name exists: a dropped AAAA stalls both Go's resolver and Ruby's), answers with a name pointer to the question, an SOA in the authority section of every
// negative answer, the AA and RA bits set (Go's resolver treats a NOERROR answer with neither as a lame referral). No EDNS, no compression beyond the one pointer.

const (
	typeA            uint16 = 1
	typeNS           uint16 = 2
	typeSOA          uint16 = 6
	typePTR          uint16 = 12
	typeMX           uint16 = 15
	typeTXT          uint16 = 16
	typeAAAA         uint16 = 28
	typeANY          uint16 = 255
	classIN          uint16 = 1
	classANY         uint16 = 255
	flagQR                  = 0x8000
	flagAA                  = 0x0400
	flagTC                  = 0x0200
	flagRD                  = 0x0100
	flagRA                  = 0x0080
	rcodeOK                 = 0
	rcodeForm               = 1
	rcodeFail               = 2
	rcodeNX                 = 3
	rcodeNotImpl            = 4
	rcodeRefused            = 5
	maxUDPPayload           = 512
	forwardDeadline         = 1500 * time.Millisecond
	dnsTCPIdle              = 10 * time.Second
	soaMinimumFloor         = 1
	rootSOAName             = "a.root-servers.invalid"
	rootSOAMailbox          = "hostmaster.root-servers.invalid"
	zoneSOAHostLabel        = "ns"
	zoneSOAMailLabel        = "hostmaster"
)

var typeByName = map[string]uint16{"A": typeA, "NS": typeNS, "SOA": typeSOA, "PTR": typePTR, "MX": typeMX, "TXT": typeTXT, "AAAA": typeAAAA, "ANY": typeANY}

func typeName(t uint16) string {
	for n, v := range typeByName {
		if v == t {
			return n
		}
	}
	return fmt.Sprintf("TYPE%d", t)
}

// ---- names ---------------------------------------------------------------------------------------------------------------------------------------------

func encodeName(name string) ([]byte, error) {
	name = strings.TrimSuffix(strings.TrimSpace(name), ".")
	if name == "" {
		return []byte{0}, nil
	}
	if len(name) > 253 {
		return nil, fmt.Errorf("the name %q is longer than 253 bytes", name)
	}
	var out []byte
	for _, l := range strings.Split(name, ".") {
		if l == "" || len(l) > 63 {
			return nil, fmt.Errorf("the name %q has an empty label or a label longer than 63 bytes", name)
		}
		out = append(out, byte(len(l)))
		out = append(out, l...)
	}
	return append(out, 0), nil
}

// decodeName reads a possibly compressed name at off and returns it lower-cased without the trailing dot, and the offset after it.
func decodeName(msg []byte, off int) (string, int, error) {
	var labels []string
	next, hops, jumped := 0, 0, false
	for {
		if off >= len(msg) {
			return "", 0, errors.New("name runs past the packet")
		}
		l := int(msg[off])
		switch {
		case l == 0:
			if !jumped {
				next = off + 1
			}
			return strings.ToLower(strings.Join(labels, ".")), next, nil
		case l&0xC0 == 0xC0:
			if off+1 >= len(msg) {
				return "", 0, errors.New("truncated name pointer")
			}
			ptr := int(binary.BigEndian.Uint16(msg[off:]) & 0x3FFF)
			if !jumped {
				next = off + 2
			}
			jumped = true
			if hops++; hops > 10 || ptr >= len(msg) {
				return "", 0, errors.New("bad name pointer")
			}
			off = ptr
		case l&0xC0 != 0:
			return "", 0, errors.New("bad label type")
		default:
			if off+1+l > len(msg) {
				return "", 0, errors.New("label runs past the packet")
			}
			labels = append(labels, string(msg[off+1:off+1+l]))
			off += 1 + l
		}
	}
}

// ---- a query and its answer --------------------------------------------------------------------------------------------------------------------------

type dnsQuery struct {
	id       uint16
	flags    uint16
	name     string
	qtype    uint16
	qclass   uint16
	question []byte // the question section exactly as received, echoed in the answer
}

// parseQuery returns the query, or (rcode, false) when the packet cannot be answered normally: rcode -1 means "ignore it" (too short, or already a response).
func parseQuery(pkt []byte) (dnsQuery, int, bool) {
	if len(pkt) < 12 {
		return dnsQuery{}, -1, false
	}
	q := dnsQuery{id: binary.BigEndian.Uint16(pkt), flags: binary.BigEndian.Uint16(pkt[2:])}
	if q.flags&flagQR != 0 {
		return q, -1, false
	}
	if (q.flags>>11)&0xF != 0 {
		return q, rcodeNotImpl, false
	}
	if binary.BigEndian.Uint16(pkt[4:]) != 1 {
		return q, rcodeForm, false
	}
	name, off, err := decodeName(pkt, 12)
	if err != nil || off+4 > len(pkt) {
		return q, rcodeForm, false
	}
	q.name = name
	q.qtype = binary.BigEndian.Uint16(pkt[off:])
	q.qclass = binary.BigEndian.Uint16(pkt[off+2:])
	q.question = append([]byte(nil), pkt[12:off+4]...)
	return q, 0, true
}

type rr struct {
	typ   uint16
	rdata []byte
}

type dnsResult struct {
	kind    string // answer, nodata, nxdomain, servfail, refused, silent, forward
	rcode   int
	aa      bool
	answers []rr
	soaZone string // apex whose SOA goes into the authority section ("" with soa=false: none)
	soa     bool
}

func rdataA(v string) ([]byte, error) {
	ip := net.ParseIP(v)
	if ip == nil || ip.To4() == nil {
		return nil, fmt.Errorf("%q is not an IPv4 address", v)
	}
	return ip.To4(), nil
}

func rdataMX(v string) ([]byte, error) {
	p, host, err := parseMX(v)
	if err != nil {
		return nil, err
	}
	n, err := encodeName(host)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 2, 2+len(n))
	binary.BigEndian.PutUint16(out, p)
	return append(out, n...), nil
}

func rdataTXT(v string) []byte {
	if v == "" {
		return []byte{0}
	}
	var out []byte
	for len(v) > 0 {
		n := len(v)
		if n > 255 {
			n = 255
		}
		out = append(out, byte(n))
		out = append(out, v[:n]...)
		v = v[n:]
	}
	return out
}

func rdataSOA(mname, rname string, serial, minimum uint32) []byte {
	m, _ := encodeName(mname)
	r, _ := encodeName(rname)
	out := append(m, r...)
	for _, v := range []uint32{serial, 3600, 600, 86400, minimum} {
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], v)
		out = append(out, b[:]...)
	}
	return out
}

func (s *state) zoneFor(name string) string {
	for _, z := range s.zones { // longest first
		if name == z || strings.HasSuffix(name, "."+z) {
			return z
		}
	}
	return ""
}

func (s *state) modeFor(name string) string {
	for n := name; ; {
		if m, ok := s.modes[n]; ok {
			return m
		}
		i := strings.IndexByte(n, '.')
		if i < 0 {
			return ""
		}
		n = n[i+1:]
	}
}

func (s *state) nameExists(name string) bool {
	if _, ok := s.records[name]; ok {
		return true
	}
	suffix := "." + name
	for n := range s.records {
		if strings.HasSuffix(n, suffix) {
			return true // an empty non-terminal: NODATA, not NXDOMAIN
		}
	}
	return false
}

// resolve decides what to say. It is the whole behaviour of the DNS side; the packet code around it only encodes the result.
func (s *state) resolve(name string, qtype uint16) dnsResult {
	s.mu.RLock()
	defer s.mu.RUnlock()
	zone := s.zoneFor(name)
	switch s.modeFor(name) {
	case "servfail":
		return dnsResult{kind: "servfail", rcode: rcodeFail}
	case "refused":
		return dnsResult{kind: "refused", rcode: rcodeRefused}
	case "silent":
		return dnsResult{kind: "silent"}
	case "nxdomain":
		return dnsResult{kind: "nxdomain", rcode: rcodeNX, aa: true, soa: true, soaZone: zone}
	}
	if zone == "" {
		if !strings.Contains(name, ".") && name != "" && s.upstream != "" {
			return dnsResult{kind: "forward"}
		}
		return dnsResult{kind: "nxdomain", rcode: rcodeNX, aa: true, soa: true}
	}
	if !s.nameExists(name) {
		return dnsResult{kind: "nxdomain", rcode: rcodeNX, aa: true, soa: true, soaZone: zone}
	}
	sets := s.records[name]
	var types []string
	for t := range sets {
		if qtype == typeANY || typeByName[t] == qtype {
			types = append(types, t)
		}
	}
	sort.Strings(types)
	var answers []rr
	for _, t := range types {
		for _, v := range sets[t] {
			var rd []byte
			var err error
			switch t {
			case "A":
				rd, err = rdataA(v)
			case "MX":
				rd, err = rdataMX(v)
			case "PTR":
				rd, err = encodeName(v)
			case "TXT":
				rd = rdataTXT(v)
			}
			if err != nil {
				return dnsResult{kind: "servfail", rcode: rcodeFail} // unreachable: every value was validated on the way in
			}
			answers = append(answers, rr{typ: typeByName[t], rdata: rd})
		}
	}
	if qtype == typeSOA || (qtype == typeANY && name == zone) {
		answers = append(answers, rr{typ: typeSOA, rdata: s.soaData(zone)})
	}
	if len(answers) == 0 {
		return dnsResult{kind: "nodata", rcode: rcodeOK, aa: true, soa: true, soaZone: zone}
	}
	return dnsResult{kind: "answer", rcode: rcodeOK, aa: true, answers: answers}
}

func (s *state) soaData(zone string) []byte {
	min := s.ttl
	if min < soaMinimumFloor {
		min = soaMinimumFloor
	}
	if zone == "" {
		return rdataSOA(rootSOAName, rootSOAMailbox, 1, min)
	}
	return rdataSOA(zoneSOAHostLabel+"."+zone, zoneSOAMailLabel+"."+zone, 1, min)
}

func (s *state) build(q dnsQuery, res dnsResult) []byte {
	flags := uint16(flagQR|flagRA) | (q.flags & flagRD) | uint16(res.rcode&0xF)
	if res.aa {
		flags |= flagAA
	}
	var body []byte
	for _, a := range res.answers {
		body = append(body, 0xC0, 0x0C)
		body = binary.BigEndian.AppendUint16(body, a.typ)
		body = binary.BigEndian.AppendUint16(body, classIN)
		body = binary.BigEndian.AppendUint32(body, s.ttl)
		body = binary.BigEndian.AppendUint16(body, uint16(len(a.rdata)))
		body = append(body, a.rdata...)
	}
	ns := 0
	if res.soa {
		zone := res.soaZone
		owner, _ := encodeName(zone)
		body = append(body, owner...)
		body = binary.BigEndian.AppendUint16(body, typeSOA)
		body = binary.BigEndian.AppendUint16(body, classIN)
		body = binary.BigEndian.AppendUint32(body, s.ttl)
		rd := s.soaData(zone)
		body = binary.BigEndian.AppendUint16(body, uint16(len(rd)))
		body = append(body, rd...)
		ns = 1
	}
	out := make([]byte, 12, 12+len(q.question)+len(body))
	binary.BigEndian.PutUint16(out[0:], q.id)
	binary.BigEndian.PutUint16(out[2:], flags)
	binary.BigEndian.PutUint16(out[4:], 1)
	binary.BigEndian.PutUint16(out[6:], uint16(len(res.answers)))
	binary.BigEndian.PutUint16(out[8:], uint16(ns))
	out = append(out, q.question...)
	return append(out, body...)
}

// errorReply is a header-only answer (FORMERR, NOTIMP, or SERVFAIL for a query we could parse only partly).
func errorReply(pkt []byte, rcode int) []byte {
	out := make([]byte, 12)
	if len(pkt) >= 2 {
		copy(out, pkt[:2])
	}
	flags := uint16(flagQR|flagRA) | uint16(rcode&0xF)
	if len(pkt) >= 4 {
		flags |= binary.BigEndian.Uint16(pkt[2:]) & flagRD
	}
	binary.BigEndian.PutUint16(out[2:], flags)
	return out
}

// answerPacket handles one query packet: the reply to send, or nil to stay silent. proto and remote are for the log only.
func (s *state) answerPacket(pkt []byte, proto, remote string) []byte {
	q, rc, ok := parseQuery(pkt)
	if !ok {
		if rc < 0 {
			return nil
		}
		kind := "formerr"
		if rc == rcodeNotImpl {
			kind = "notimp"
		}
		s.logDNS(dnsEntry{Remote: remote, Proto: proto, Name: "?", Type: "?", Kind: kind})
		return errorReply(pkt, rc)
	}
	if q.qclass != classIN && q.qclass != classANY {
		s.logDNS(dnsEntry{Remote: remote, Proto: proto, Name: q.name, Type: typeName(q.qtype), Kind: "refused"})
		return s.build(q, dnsResult{rcode: rcodeRefused})
	}
	res := s.resolve(q.name, q.qtype)
	s.logDNS(dnsEntry{Remote: remote, Proto: proto, Name: q.name, Type: typeName(q.qtype), Kind: res.kind})
	switch res.kind {
	case "silent":
		return nil
	case "forward":
		if reply, err := s.forward(pkt); err == nil {
			return reply
		}
		return s.build(q, dnsResult{kind: "servfail", rcode: rcodeFail})
	}
	return s.build(q, res)
}

// forward relays the query unchanged to the upstream resolver (Docker's embedded one) and returns its answer unchanged.
func (s *state) forward(pkt []byte) ([]byte, error) {
	c, err := net.DialTimeout("udp", s.upstream, forwardDeadline)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(forwardDeadline))
	if _, err := c.Write(pkt); err != nil {
		return nil, err
	}
	buf := make([]byte, 4096)
	n, err := c.Read(buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

// ---- the servers -----------------------------------------------------------------------------------------------------------------------------------------

type dnsServer struct {
	st    *state
	udp   net.PacketConn
	tcp   net.Listener
	wg    sync.WaitGroup
	conns sync.Map
}

func newDNSServer(st *state, udp net.PacketConn, tcp net.Listener) *dnsServer {
	return &dnsServer{st: st, udp: udp, tcp: tcp}
}

func (d *dnsServer) serve() {
	d.wg.Add(2)
	go func() { defer d.wg.Done(); d.serveUDP() }()
	go func() { defer d.wg.Done(); d.serveTCP() }()
}

func (d *dnsServer) close() {
	_ = d.udp.Close()
	_ = d.tcp.Close()
	d.conns.Range(func(k, _ any) bool { _ = k.(net.Conn).Close(); return true })
	d.wg.Wait()
}

func (d *dnsServer) serveUDP() {
	buf := make([]byte, 4096)
	for {
		n, addr, err := d.udp.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		pkt := append([]byte(nil), buf[:n]...)
		go func() {
			reply := d.st.answerPacket(pkt, "udp", hostOf(addr.String()))
			if reply == nil {
				return
			}
			if len(reply) > maxUDPPayload && len(reply) > 12 { // never reached by the shipped records; truncated rather than fragmented if it ever is
				reply = append([]byte(nil), reply[:12]...)
				binary.BigEndian.PutUint16(reply[2:], binary.BigEndian.Uint16(reply[2:])|flagTC)
			}
			_, _ = d.udp.WriteTo(reply, addr)
		}()
	}
}

func (d *dnsServer) serveTCP() {
	for {
		c, err := d.tcp.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		d.conns.Store(c, struct{}{})
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			defer d.conns.Delete(c)
			defer c.Close()
			d.handleTCP(c)
		}()
	}
}

func (d *dnsServer) handleTCP(c net.Conn) {
	r := bufio.NewReader(c)
	for {
		_ = c.SetReadDeadline(time.Now().Add(dnsTCPIdle))
		var lb [2]byte
		if _, err := io.ReadFull(r, lb[:]); err != nil {
			return
		}
		n := int(binary.BigEndian.Uint16(lb[:]))
		if n < 12 {
			return
		}
		pkt := make([]byte, n)
		if _, err := io.ReadFull(r, pkt); err != nil {
			return
		}
		reply := d.st.answerPacket(pkt, "tcp", hostOf(c.RemoteAddr().String()))
		if reply == nil {
			continue // silent: read on until the client gives up
		}
		out := binary.BigEndian.AppendUint16(nil, uint16(len(reply)))
		_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if _, err := c.Write(append(out, reply...)); err != nil {
			return
		}
	}
}

func hostOf(addr string) string {
	h, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return h
}

// ---- a minimal client (the control checks and the tests use it) --------------------------------------------------------------------------------

type dnsAnswer struct {
	Type  string
	Value string
}

type dnsReply struct {
	ID        uint16
	Flags     uint16
	Rcode     int
	AA, RA    bool
	TC        bool
	Answers   []dnsAnswer
	Authority []dnsAnswer
}

func buildQuery(id uint16, name string, qtype uint16) []byte {
	out := make([]byte, 12)
	binary.BigEndian.PutUint16(out[0:], id)
	binary.BigEndian.PutUint16(out[2:], flagRD)
	binary.BigEndian.PutUint16(out[4:], 1)
	n, _ := encodeName(name)
	out = append(out, n...)
	out = binary.BigEndian.AppendUint16(out, qtype)
	return binary.BigEndian.AppendUint16(out, classIN)
}

func parseReply(pkt []byte) (dnsReply, error) {
	var r dnsReply
	if len(pkt) < 12 {
		return r, errors.New("short reply")
	}
	r.ID = binary.BigEndian.Uint16(pkt)
	r.Flags = binary.BigEndian.Uint16(pkt[2:])
	r.Rcode = int(r.Flags & 0xF)
	r.AA, r.RA, r.TC = r.Flags&flagAA != 0, r.Flags&flagRA != 0, r.Flags&flagTC != 0
	qd, an, ns := int(binary.BigEndian.Uint16(pkt[4:])), int(binary.BigEndian.Uint16(pkt[6:])), int(binary.BigEndian.Uint16(pkt[8:]))
	off := 12
	for i := 0; i < qd; i++ {
		_, next, err := decodeName(pkt, off)
		if err != nil {
			return r, err
		}
		off = next + 4
	}
	read := func(count int) ([]dnsAnswer, error) {
		var out []dnsAnswer
		for i := 0; i < count; i++ {
			_, next, err := decodeName(pkt, off)
			if err != nil || next+10 > len(pkt) {
				return nil, errors.New("bad record")
			}
			typ := binary.BigEndian.Uint16(pkt[next:])
			rdlen := int(binary.BigEndian.Uint16(pkt[next+8:]))
			start := next + 10
			if start+rdlen > len(pkt) {
				return nil, errors.New("record runs past the packet")
			}
			a := dnsAnswer{Type: typeName(typ)}
			switch typ {
			case typeA:
				if rdlen == 4 {
					a.Value = net.IP(pkt[start : start+4]).String()
				}
			case typePTR:
				a.Value, _, _ = decodeName(pkt, start)
			case typeMX:
				host, _, _ := decodeName(pkt, start+2)
				a.Value = fmt.Sprintf("%d %s", binary.BigEndian.Uint16(pkt[start:]), host)
			case typeTXT:
				var sb strings.Builder
				for p := start; p < start+rdlen; {
					l := int(pkt[p])
					if p+1+l > start+rdlen {
						break
					}
					sb.Write(pkt[p+1 : p+1+l])
					p += 1 + l
				}
				a.Value = sb.String()
			case typeSOA:
				a.Value, _, _ = decodeName(pkt, start) // the primary name server
			}
			out = append(out, a)
			off = start + rdlen
		}
		return out, nil
	}
	var err error
	if r.Answers, err = read(an); err != nil {
		return r, err
	}
	r.Authority, err = read(ns)
	return r, err
}

// dnsExchange sends one query over UDP and waits up to timeout for the answer; a timeout is returned as an error (that is what "silent" looks like).
func dnsExchange(ctx context.Context, addr, name string, qtype uint16, timeout time.Duration) (dnsReply, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "udp", addr)
	if err != nil {
		return dnsReply{}, err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(timeout))
	const id = 0x4658
	if _, err := c.Write(buildQuery(id, name, qtype)); err != nil {
		return dnsReply{}, err
	}
	buf := make([]byte, 4096)
	n, err := c.Read(buf)
	if err != nil {
		return dnsReply{}, err
	}
	r, err := parseReply(buf[:n])
	if err == nil && r.ID != id {
		err = fmt.Errorf("the answer carries id %#x, the query %#x", r.ID, id)
	}
	return r, err
}

// dnsExchangeTCP is the same over TCP.
func dnsExchangeTCP(ctx context.Context, addr, name string, qtype uint16, timeout time.Duration) (dnsReply, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return dnsReply{}, err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(timeout))
	q := buildQuery(0x4659, name, qtype)
	if _, err := c.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(q))), q...)); err != nil {
		return dnsReply{}, err
	}
	var lb [2]byte
	if _, err := io.ReadFull(c, lb[:]); err != nil {
		return dnsReply{}, err
	}
	buf := make([]byte, binary.BigEndian.Uint16(lb[:]))
	if _, err := io.ReadFull(c, buf); err != nil {
		return dnsReply{}, err
	}
	return parseReply(buf)
}
