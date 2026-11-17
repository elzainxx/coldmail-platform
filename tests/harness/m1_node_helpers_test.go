//go:build harness && m1node

package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	m1results "github.com/coldmail-platform/platform/internal/adapters/m1"
)

// The helpers of the module-1 node tests (m1_node_test.go). Behind the build tag m1node: the plain `-tags harness` build, the db-tests and the full harness never compile them.
//
// What these tests talk to: module 1's WIRED control plane (service m1n: HARNESS_M1_URL / HARNESS_M1_KEY, the per-caller key run.sh seeded), the fixture's control API
// (HARNESS_M1NODE_FX_URL: the SMTP sessions the node opened, the DNS queries module 1 and the node asked, the switches that change the fixture's behaviour for one name), and the
// addresses run.sh derived from the fixture network (HARNESS_M1NODE_NODE_IP, HARNESS_M1NODE_SMTP_IP).

const (
	m1nEnvFx     = "HARNESS_M1NODE_FX_URL"
	m1nEnvNodeID = "HARNESS_M1NODE_NODE_ID"
	m1nEnvNodeIP = "HARNESS_M1NODE_NODE_IP"
	m1nEnvSmtpIP = "HARNESS_M1NODE_SMTP_IP"

	m1nJobWait   = 5 * time.Minute // one bulk job of at most ~25 rows, the slowest of them a 2 s DNS silence or a 3 s SMTP stall
	m1nPollEvery = 300 * time.Millisecond
)

// ---- the environment ---------------------------------------------------------------------------------------------------------------------------------

type m1nEnv struct {
	t      *testing.T
	ctx    context.Context
	m1     *m1results.Adapter // the platform's own live client: Submit and Job (and Drain, for the cross-check of the table)
	base   string
	key    string
	hc     *http.Client
	fx     *m1nFx
	nodeID string
	nodeIP string
	smtpIP string
}

func newM1nEnv(t *testing.T) *m1nEnv {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	t.Cleanup(cancel)
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil // a per-caller key must never be handed to a proxy
	hc := &http.Client{Timeout: 45 * time.Second, Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	e := &m1nEnv{
		t: t, ctx: ctx, m1: m1bClient(t), base: m3lBaseURL(t, m1bEnvURL), key: strings.TrimSpace(requireEnv(t, m1bEnvKey)), hc: hc,
		fx:     &m1nFx{t: t, base: m3lBaseURL(t, m1nEnvFx), hc: &http.Client{Timeout: 15 * time.Second, Transport: tr}},
		nodeID: strings.TrimSpace(requireEnv(t, m1nEnvNodeID)), nodeIP: strings.TrimSpace(requireEnv(t, m1nEnvNodeIP)), smtpIP: strings.TrimSpace(requireEnv(t, m1nEnvSmtpIP)),
	}
	e.m1.PageLimit = 7 // several pages for a job of twenty-odd rows
	e.m1.Poll = m1nPollEvery
	return e
}

// ---- the fixture's control API ------------------------------------------------------------------------------------------------------------------------

type m1nFx struct {
	t    *testing.T
	base string
	hc   *http.Client
}

type m1nSession struct {
	Seq     int    `json:"seq"`
	Session int    `json:"session"`
	Remote  string `json:"remote"`
	Rcpt    string `json:"rcpt"`
	Domain  string `json:"domain"`
	Rule    string `json:"rule"`
	Action  string `json:"action"`
	Reply   string `json:"reply"`
}

type m1nDNSQuery struct {
	Seq    int    `json:"seq"`
	Remote string `json:"remote"`
	Name   string `json:"name"`
	Type   string `json:"type"`
	Kind   string `json:"kind"`
}

type m1nRule struct {
	Match   string `json:"match"`
	Action  string `json:"action,omitempty"`
	Reply   string `json:"reply,omitempty"`
	StallMs int    `json:"stall_ms,omitempty"`
}

func (f *m1nFx) do(method, path string, body any, out any) int {
	f.t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			f.t.Fatalf("encoding the body of %s %s: %v", method, path, err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, f.base+path, rd)
	if err != nil {
		f.t.Fatalf("%s %s: %v", method, path, err)
	}
	resp, err := f.hc.Do(req)
	if err != nil {
		f.t.Fatalf("the fixture's control API did not answer %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if out != nil && resp.StatusCode == 200 {
		if err := json.Unmarshal(raw, out); err != nil {
			f.t.Fatalf("%s %s: the answer is not the documented JSON: %v", method, path, err)
		}
	}
	if resp.StatusCode != 200 {
		f.t.Fatalf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return resp.StatusCode
}

// seq is the sequence number of the fixture's latest log line: the baseline for "what happened since".
func (f *m1nFx) seq() int {
	f.t.Helper()
	var st struct {
		Seq int `json:"seq"`
	}
	f.do(http.MethodGet, "/api/state", nil, &st)
	return st.Seq
}

func (f *m1nFx) sessions(since int, domain string) []m1nSession {
	f.t.Helper()
	q := url.Values{"since": {strconv.Itoa(since)}}
	if domain != "" {
		q.Set("domain", domain)
	}
	var out []m1nSession
	f.do(http.MethodGet, "/api/smtp/sessions?"+q.Encode(), nil, &out)
	return out
}

func (f *m1nFx) dnsQueries(since int) []m1nDNSQuery {
	f.t.Helper()
	var out []m1nDNSQuery
	f.do(http.MethodGet, "/api/dns/queries?since="+strconv.Itoa(since), nil, &out)
	return out
}

func (f *m1nFx) setRules(domain string, rules ...m1nRule) {
	f.t.Helper()
	f.do(http.MethodPut, "/api/smtp/domains", map[string]any{"domain": domain, "rules": rules}, nil)
}

func (f *m1nFx) setMode(name, mode string) {
	f.t.Helper()
	f.do(http.MethodPut, "/api/dns/modes", map[string]string{"name": name, "mode": mode}, nil)
}

func (f *m1nFx) setA(name string, ips ...string) {
	f.t.Helper()
	f.do(http.MethodPut, "/api/dns/records", map[string]any{"name": name, "type": "A", "values": ips}, nil)
}

const (
	m1nUnknownUser = "550 5.1.1 <{addr}> User unknown"
	m1nAccept      = "250 2.1.5 OK"
)

// m1nBaseRules is the behaviour of an ordinary recipient domain: the probe address is refused, anything else accepted.
func m1nBaseRules() []m1nRule {
	return []m1nRule{{Match: "nx-*", Reply: m1nUnknownUser}, {Match: "*", Reply: m1nAccept}}
}

// ---- module 1: a raw reader of the results, a synchronous verify, a bulk job run to its end ---------------------------------------------------------------------

// m1nRow is one result element with the signals the platform's own Row drops: the fixtures' proofs are in them.
type m1nRow struct {
	Ordinal        int             `json:"ordinal"`
	ItemID         string          `json:"item_id"`
	DuplicateOf    *int            `json:"duplicate_of"`
	Status         string          `json:"status"`
	Reason         string          `json:"reason"`
	Confidence     int             `json:"confidence"`
	Recommendation string          `json:"recommendation"`
	Flags          map[string]bool `json:"flags"`
	Signals        map[string]any  `json:"signals"`
}

func (e *m1nEnv) getJSON(path string, out any) int {
	e.t.Helper()
	req, _ := http.NewRequestWithContext(e.ctx, http.MethodGet, e.base+path, nil)
	req.Header.Set("Authorization", "Bearer "+e.key)
	resp, err := e.hc.Do(req)
	if err != nil {
		e.t.Fatalf("GET %s: %v", strings.SplitN(path, "?", 2)[0], err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode == 200 && out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			e.t.Fatalf("GET %s: not the documented JSON: %v", strings.SplitN(path, "?", 2)[0], err)
		}
	}
	return resp.StatusCode
}

// rawResults reads a COMPLETE job's listing page by page (immutable once complete), in ordinal order.
func (e *m1nEnv) rawResults(jobID string) []m1nRow {
	e.t.Helper()
	var all []m1nRow
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 50 {
			e.t.Fatalf("the results of job %s did not end after %d pages", jobID, pages)
		}
		q := url.Values{"limit": {"8"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var page struct {
			Results    []m1nRow `json:"results"`
			NextCursor string   `json:"next_cursor"`
		}
		if code := e.getJSON("/v1/jobs/"+jobID+"/results?"+q.Encode(), &page); code != 200 {
			e.t.Fatalf("GET /v1/jobs/%s/results answered HTTP %d", jobID, code)
		}
		all = append(all, page.Results...)
		if page.NextCursor == "" {
			return all
		}
		if page.NextCursor == cursor {
			e.t.Fatalf("the results cursor of job %s did not advance", jobID)
		}
		cursor = page.NextCursor
	}
}

// runJob submits the rows as ONE bulk job, waits until module 1 says it is complete, and returns the 202's counts and the rows by item_id.
func (e *m1nEnv) runJob(items []m1results.Item) (string, m1results.SubmitCounts, map[string]m1nRow) {
	e.t.Helper()
	jobID, counts, err := e.m1.SubmitRaw(e.ctx, items)
	if err != nil {
		e.t.Fatalf("submitting the bulk job to module 1 failed: %v", err)
	}
	ctx, cancel := context.WithTimeout(e.ctx, m1nJobWait)
	defer cancel()
	var js m1results.JobStatus
	for {
		js, err = e.m1.Status(ctx, jobID)
		if err != nil {
			e.t.Fatalf("reading the job %s failed: %v", jobID, err)
		}
		if js.Complete {
			break
		}
		select {
		case <-ctx.Done():
			e.t.Fatalf("job %s of %d row(s) was not complete after %v: module 1 says status %q, total %d, settled %d, pending %d (a wired node that is not eligible makes a job wait: TM_QUEUE_MAX_RETRY_AGE_SECONDS=90 ends the wait)", jobID, len(items), m1nJobWait, js.Status, js.Counts.Total, js.Counts.Settled, js.Counts.Pending)
		case <-time.After(m1nPollEvery):
		}
	}
	if js.Counts.Total != len(items) || js.Counts.Settled != len(items) || js.Counts.Pending != 0 {
		e.t.Fatalf("job %s is complete but its counts are %+v for %d row(s)", jobID, js.Counts, len(items))
	}
	rows := e.rawResults(jobID)
	if len(rows) != len(items) {
		e.t.Fatalf("job %s: %d result(s) for %d input row(s)", jobID, len(rows), len(items))
	}
	byItem := map[string]m1nRow{}
	for i, r := range rows {
		if r.Ordinal != i || r.ItemID != items[i].ItemID {
			e.t.Fatalf("job %s: result %d is ordinal %d item_id %q, want ordinal %d item_id %q: a result must attribute to exactly one input row, in order", jobID, i, r.Ordinal, r.ItemID, i, items[i].ItemID)
		}
		byItem[r.ItemID] = r
	}
	return jobID, counts, byItem
}

type m1nSyncVerdict struct {
	Status     string          `json:"status"`
	Reason     string          `json:"reason"`
	Confidence int             `json:"confidence"`
	Flags      map[string]bool `json:"flags"`
	Signals    map[string]any  `json:"signals"`
	Meta       struct {
		NodeID     *string `json:"node_id"`
		VerifierIP *string `json:"verifier_ip"`
		FromCache  bool    `json:"from_cache"`
	} `json:"meta"`
}

// syncVerify is POST /v1/verify through module 1 (the synchronous door).
func (e *m1nEnv) syncVerify(email string) m1nSyncVerdict {
	e.t.Helper()
	b, _ := json.Marshal(map[string]string{"email": email})
	req, _ := http.NewRequestWithContext(e.ctx, http.MethodPost, e.base+"/v1/verify", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+e.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.hc.Do(req)
	if err != nil {
		e.t.Fatalf("POST /v1/verify: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != 200 {
		e.t.Fatalf("POST /v1/verify answered HTTP %d", resp.StatusCode)
	}
	var v m1nSyncVerdict
	if err := json.Unmarshal(raw, &v); err != nil {
		e.t.Fatalf("POST /v1/verify: not the documented JSON: %v", err)
	}
	return v
}

// ---- expectations --------------------------------------------------------------------------------------------------------------------------------------

// m1nWant is what a row must be, EXACTLY: the verdict, its confidence, who failed (signals.failure_origin; "" = the key must be absent), the signals that must hold these values,
// the signals that must be absent, and the flags that must hold. flags.from_cache is false unless the want says otherwise (flags["from_cache"]).
type m1nWant struct {
	status, reason string
	conf           int
	origin         string
	sig            map[string]any
	absent         []string
	flags          map[string]bool
}

// sigEqual compares one decoded JSON value with a wanted Go value: string, bool, int (JSON numbers decode to float64), []string (a JSON array of strings, in order).
func sigEqual(got, want any) bool {
	switch w := want.(type) {
	case string:
		g, ok := got.(string)
		return ok && g == w
	case bool:
		g, ok := got.(bool)
		return ok && g == w
	case int:
		g, ok := got.(float64)
		return ok && g == float64(w)
	case []string:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			return false
		}
		for i := range w {
			if s, ok := g[i].(string); !ok || s != w[i] {
				return false
			}
		}
		return true
	}
	return false
}

// check returns every way the row differs from the want; nothing when it is exactly as wanted.
func (w m1nWant) check(r m1nRow) []string {
	var p []string
	if r.Status != w.status || r.Reason != w.reason {
		p = append(p, fmt.Sprintf("verdict %s/%s, want %s/%s", r.Status, r.Reason, w.status, w.reason))
	}
	if r.Confidence != w.conf {
		p = append(p, fmt.Sprintf("confidence %d, want %d", r.Confidence, w.conf))
	}
	got, has := r.Signals["failure_origin"]
	switch {
	case w.origin == "" && has:
		p = append(p, fmt.Sprintf("signals.failure_origin is %v, want the key absent", got))
	case w.origin != "" && !has:
		p = append(p, fmt.Sprintf("signals.failure_origin is absent, want %q", w.origin))
	case w.origin != "" && got != w.origin:
		p = append(p, fmt.Sprintf("signals.failure_origin is %v, want %q", got, w.origin))
	}
	for _, k := range m1nSortedKeys(w.sig) {
		g, ok := r.Signals[k]
		if !ok {
			p = append(p, fmt.Sprintf("signals.%s is absent, want %v", k, w.sig[k]))
		} else if !sigEqual(g, w.sig[k]) {
			p = append(p, fmt.Sprintf("signals.%s is %v, want %v", k, g, w.sig[k]))
		}
	}
	for _, k := range w.absent {
		if g, ok := r.Signals[k]; ok {
			p = append(p, fmt.Sprintf("signals.%s is present (%v), want it absent", k, g))
		}
	}
	for _, k := range m1nSortedKeys(w.flags) {
		if r.Flags[k] != w.flags[k] {
			p = append(p, fmt.Sprintf("flags.%s is %t, want %t", k, r.Flags[k], w.flags[k]))
		}
	}
	if _, set := w.flags["from_cache"]; !set && r.Flags["from_cache"] {
		p = append(p, "flags.from_cache is true, want false")
	}
	return p
}

func m1nSortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// short is one row on one line for the log: what the lead reads first.
func (r m1nRow) short() string {
	sig := func(k string) string {
		if v, ok := r.Signals[k]; ok {
			return fmt.Sprintf(" %s=%v", k, v)
		}
		return ""
	}
	return fmt.Sprintf("%s/%s conf=%d%s%s%s%s%s%s%s%s%s", r.Status, r.Reason, r.Confidence, sig("failure_origin"), sig("smtp_code"), sig("smtp_enhanced_code"), sig("catchall_conclusion"),
		sig("smtp_skipped_reason"), sig("greylisted"), sig("retry_eligible"), sig("probe_refused_ranges"), sig("engine_mx_hosts"))
}

// ---- the wants of every recipe (one place: the table of the bulk test and the controls read the same ones) ----------------------------------------------------------

func (e *m1nEnv) hosts(ips ...string) []string { return ips }

// wantValid: the node asked the fixture, the target was accepted, the probe refused: module 1 mints valid/accepted.
func (e *m1nEnv) wantValid() m1nWant {
	return m1nWant{status: "valid", reason: "accepted", conf: 90, sig: map[string]any{"catchall_conclusion": "not_catchall", "engine_mx_hosts": e.hosts(e.smtpIP), "brain_mx_provider": "self"},
		absent: []string{"smtp_code", "smtp_enhanced_code", "smtp_skipped_reason", "probe_refused", "greylisted", "retry_eligible"}}
}

func (e *m1nEnv) wantFull() m1nWant {
	return m1nWant{status: "valid", reason: "mailbox_full", conf: 90, sig: map[string]any{"smtp_code": "552", "smtp_enhanced_code": "5.2.2", "engine_mx_hosts": e.hosts(e.smtpIP)}, absent: []string{"catchall_conclusion"}}
}

func (e *m1nEnv) wantGone() m1nWant {
	return m1nWant{status: "invalid", reason: "mailbox_not_found", conf: 95, sig: map[string]any{"smtp_code": "550", "smtp_enhanced_code": "5.1.1", "engine_mx_hosts": e.hosts(e.smtpIP)}, absent: []string{"catchall_conclusion"}}
}

func (e *m1nEnv) wantCatchAll() m1nWant {
	return m1nWant{status: "risky", reason: "catch_all", conf: 40, sig: map[string]any{"catchall_conclusion": "catchall", "engine_mx_hosts": e.hosts(e.smtpIP)}, absent: []string{"smtp_code", "failure_origin"}}
}

func (e *m1nEnv) wantGrey() m1nWant {
	return m1nWant{status: "unknown", reason: "greylisted", conf: 10, origin: "remote",
		sig: map[string]any{"smtp_code": "450", "smtp_enhanced_code": "4.2.0", "greylisted": true, "retry_eligible": true, "retry_after_hint_s": 300, "engine_mx_hosts": e.hosts(e.smtpIP)}}
}

func (e *m1nEnv) wantBlocked() m1nWant {
	return m1nWant{status: "unknown", reason: "smtp_blocked", conf: 10, origin: "own",
		sig: map[string]any{"smtp_code": "554", "smtp_enhanced_code": "5.7.1", "retry_eligible": true, "engine_mx_hosts": e.hosts(e.smtpIP)}}
}

func (e *m1nEnv) wantAmbiguous() m1nWant {
	return m1nWant{status: "unknown", reason: "smtp_error", conf: 10, origin: "remote",
		sig: map[string]any{"smtp_code": "550", "smtp_enhanced_code": "5.7.0", "engine_mx_hosts": e.hosts(e.smtpIP)}, absent: []string{"retry_eligible"}}
}

func (e *m1nEnv) wantStall() m1nWant {
	return m1nWant{status: "unknown", reason: "smtp_timeout", conf: 10, origin: "remote", sig: map[string]any{"retry_eligible": true, "engine_mx_hosts": e.hosts(e.smtpIP)}, absent: []string{"smtp_code"}}
}

func (e *m1nEnv) wantDropped() m1nWant {
	return m1nWant{status: "unknown", reason: "smtp_error", conf: 10, origin: "undetermined", sig: map[string]any{"engine_mx_hosts": e.hosts(e.smtpIP)}, absent: []string{"smtp_code", "retry_eligible"}}
}

// wantNoConn: the MX host answers at the node's own address, where nothing listens on the SMTP port: refused at once.
func (e *m1nEnv) wantNoConn() m1nWant {
	return m1nWant{status: "unknown", reason: "connection_failed", conf: 10, origin: "undetermined", sig: map[string]any{"retry_eligible": true, "engine_mx_hosts": e.hosts(e.nodeIP)}, absent: []string{"smtp_code"}}
}

func (e *m1nEnv) wantNonPublic() m1nWant {
	return m1nWant{status: "risky", reason: "non_public_mx", conf: 20, sig: map[string]any{"probe_refused": true, "probe_refused_count": 1, "probe_refused_ranges": []string{"private-rfc1918"}}, absent: []string{"failure_origin", "smtp_code"}}
}

func (e *m1nEnv) wantProvider() m1nWant {
	return m1nWant{status: "risky", reason: "provider_no_smtp", conf: 35, sig: map[string]any{"smtp_skipped_reason": "provider_mx_routed"}, absent: []string{"failure_origin", "smtp_code", "engine_mx_hosts"}}
}

func wantNoMX() m1nWant {
	return m1nWant{status: "invalid", reason: "no_mx", conf: 96, absent: []string{"failure_origin", "smtp_code", "engine_mx_hosts"}}
}

func wantNullMX() m1nWant {
	return m1nWant{status: "invalid", reason: "null_mx", conf: 97, sig: map[string]any{"null_mx": true}, absent: []string{"failure_origin", "smtp_code", "engine_mx_hosts"}}
}

func wantTrap() m1nWant {
	return m1nWant{status: "invalid", reason: "spam_trap", conf: 95, absent: []string{"failure_origin", "smtp_code", "engine_mx_hosts"}}
}

func wantDisposable() m1nWant {
	return m1nWant{status: "risky", reason: "disposable", conf: 40, flags: map[string]bool{"is_disposable": true}, absent: []string{"failure_origin", "smtp_code", "engine_mx_hosts"}}
}

func wantServfail() m1nWant {
	return m1nWant{status: "unknown", reason: "dns_error", conf: 10, origin: "remote", absent: []string{"smtp_code", "engine_mx_hosts"}}
}

func wantSilent() m1nWant {
	return m1nWant{status: "unknown", reason: "dns_error", conf: 10, origin: "own", absent: []string{"smtp_code", "engine_mx_hosts"}}
}

func wantRefused() m1nWant {
	return m1nWant{status: "unknown", reason: "dns_error", conf: 10, origin: "undetermined", absent: []string{"smtp_code", "engine_mx_hosts"}}
}

// ---- the sessions the fixture saw ----------------------------------------------------------------------------------------------------------------------------

var m1nUnique atomic.Int64

// fresh returns a local part nobody has used in this run: the cache is keyed by address, so a control that must reach the node again needs a new address.
func fresh(prefix string) string { return fmt.Sprintf("%s%03d", prefix, m1nUnique.Add(1)) }

// checkSessions says whether the fixture saw what a node-reaching verification must leave: exactly two RCPTs at the domain (the target, then the nx- probe), both from the node.
// A recipe that never reaches the node must leave none.
func (e *m1nEnv) checkSessions(sessions []m1nSession, domain, local string, reaches bool) []string {
	var mine []m1nSession
	for _, s := range sessions {
		if s.Domain == domain {
			mine = append(mine, s)
		}
	}
	var p []string
	for _, s := range mine {
		if s.Remote != e.nodeIP {
			p = append(p, fmt.Sprintf("%s: a session from %s, only the node (%s) may open one", domain, s.Remote, e.nodeIP))
		}
	}
	if !reaches {
		if len(mine) != 0 {
			p = append(p, fmt.Sprintf("%s: %d RCPT(s) seen, a recipe that never reaches the node must leave none", domain, len(mine)))
		}
		return p
	}
	var target, probe int
	for _, s := range mine {
		switch {
		case strings.HasPrefix(strings.ToLower(s.Rcpt), "nx-"):
			probe++
		case strings.EqualFold(s.Rcpt, local+"@"+domain):
			target++
		}
	}
	if len(mine) != 2 || target != 1 || probe != 1 {
		p = append(p, fmt.Sprintf("%s: %d RCPT(s) seen (%d for %s, %d nx- probe), want exactly the target and the probe", domain, len(mine), target, local, probe))
	}
	return p
}
