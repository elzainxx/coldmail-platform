//go:build harness

package harness

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"testing"
)

// Helpers of the From-gate tests of module 3's adapter (Phase 3 close-out D2, m3_adapter_closeout_fromgate_test.go) and of the shared env builder, which switches the gate on for every adapter test
// (m3_adapter_e2e_helpers_test.go, newM3aEnv). Every name starts with `m3d`.
//
// THE FIXTURE. Module 3's gate asks DNS for the From domain's SPF, its DMARC record and (for a provider whose selectors it knows) its DKIM selectors. Under the adapter tests it asks the service `m3dns`
// (tests/harness/fx with FX_ROLES=dns,api serving tests/harness/m3adapter/m3dns.json): module 3 is pointed at it by the dial from_dns_resolver (m3dResolver), the tests change and read it through its control
// API (m3dns:8025, HARNESS_M3DNS_URL). The file holds what every test shares (the default From domain of every adapter account, the SPF include targets of the two providers, one include target that is not a
// provider's); every domain a From-gate test judges is made at RUN TIME under a name of this run (fg<run>-<label>.test), because module 3 caches every lookup it makes for the life of its process (positive
// answers 3600 s, absences 300 s, both read ONCE at start: platformapi/server.go newFromGateCacheFromConfig, so a setDial of the two TTLs changes nothing until module 3 restarts) and never caches a
// failed lookup. A record set is therefore changed under a name module 3 has not yet got an answer for: a name that answered SERVFAIL has no cached answer.
//
// THE HOSTS. The gate identifies the provider from the LITERAL smtp_host (provider.go matchProvider: an exact match with the entries of its table). Two network aliases (docker-compose.yml) give the two sinks
// the match strings of two entries: smtp.fastmail.com (Mailpit) and smtp.gmail.com (the controllable sink). Both providers publish DKIM selectors the gate can probe, so BLOCK is reachable for them.
// An account at either name cannot be VERIFIED: the worker validates the certificate of a host that is not private-looking (smtp.js isPrivateHost) and the sink's names only smtp-sink and localhost. The
// tests of the From-gate read the From verdict and nothing of the credential check but that its words are in the closed sets.
const (
	m3dEnvURL    = "HARNESS_M3DNS_URL" // the fixture DNS's control API
	m3dResolver  = "m3dns:53"          // module 3's from_dns_resolver for the adapter tests
	m3dHostMail  = "smtp-sink"         // Mailpit by its service name: not in the provider table, exempt from certificate validation by its name
	m3dHostFast  = "smtp.fastmail.com" // an alias of Mailpit: Fastmail's entry (SPF include spf.messagingengine.com, DKIM selectors fm1, fm2, fm3 ._domainkey)
	m3dHostGoog  = "smtp.gmail.com"    // an alias of the controllable sink: Google Workspace's entry (SPF include _spf.google.com, DKIM selector google._domainkey)
	m3dHostCtl   = "10.smtp-ctl.example.test"
	m3dTokenFast = "spf.messagingengine.com"
	m3dTokenGoog = "_spf.google.com"
	m3dNotAToken = "spf-other.test" // an SPF include target of the fixture that belongs to no provider
)

// m3dRawDNSText is what the module's rendered sentences about a From verdict (message, from_alignment_message) must never carry: the raw record the gate read, a raw resolver error, the resolver's name.
var m3dRawDNSText = []string{"v=spf1", "v=DMARC1", "SERVFAIL", "NXDOMAIN", "m3dns", "dial udp", "i/o timeout", "server misbehaving", "no such host"}

// ---- the fixture's control API ----

// m3dQuery is one question the fixture DNS answered (GET /api/dns/queries): the name without its final dot, the type, and what it said (answer, nodata, nxdomain, servfail, refused, silent, forward).
type m3dQuery struct {
	Seq  int    `json:"seq"`
	Name string `json:"name"`
	Type string `json:"type"`
	Kind string `json:"kind"`
}

func (e *m3aEnv) dnsCall(t *testing.T, method, path string, body any, want int) []byte {
	t.Helper()
	status, out := e.raw.do(t, e.dnsURL, method, path, "", body)
	if status != want {
		t.Fatalf("the From-gate's fixture DNS (m3dns, its control API) answered HTTP %d to %s %s, want %d: %s", status, method, path, want, e.raw.snip(out))
	}
	return out
}

// requireGateFixture fails, naming what is wrong, unless the fixture DNS is up with the file this package expects and holds no name mode left over from an earlier test.
func (e *m3aEnv) requireGateFixture(t *testing.T) {
	t.Helper()
	status, body := e.raw.do(t, e.dnsURL, http.MethodGet, "/readyz", "", nil)
	if status != http.StatusOK {
		t.Fatalf("the From-gate's fixture DNS (service m3dns at %s) is not ready (HTTP %d: %s): run.sh stage 5b starts it and waits for it, and module 3 cannot decide a From verdict without it (from_dns_resolver=%s)", e.dnsURL, status, e.raw.snip(body), m3dResolver)
	}
	var st struct {
		Records map[string]map[string][]string `json:"records"`
		Modes   map[string]string              `json:"modes"`
	}
	if err := json.Unmarshal(e.dnsCall(t, http.MethodGet, "/api/dns", nil, http.StatusOK), &st); err != nil {
		t.Fatalf("the From-gate fixture's GET /api/dns is not {records, modes}: %v", err)
	}
	ex := st.Records["example.test"]
	hasMX := func(host string) bool {
		for _, v := range ex["MX"] {
			if strings.HasSuffix(v, " "+host) {
				return true
			}
		}
		return false
	}
	spfOK := false
	for _, v := range ex["TXT"] {
		if strings.HasPrefix(v, "v=spf1") && strings.Contains(" "+v+" ", " mx ") {
			spfOK = true
		}
	}
	if !spfOK || !hasMX("smtp-sink.") || !hasMX("smtp-ctl.") || len(st.Records["_dmarc.example.test"]["TXT"]) == 0 || len(st.Records[m3dTokenFast]["TXT"]) == 0 || len(st.Records[m3dTokenGoog]["TXT"]) == 0 {
		t.Fatalf("the From-gate's fixture DNS does not serve what every adapter test needs: example.test with an SPF record that has a bare mx and the MX hosts smtp-sink. and smtp-ctl., its DMARC record, and the SPF include targets %s and %s (tests/harness/m3adapter/m3dns.json); it serves %d name(s) and example.test has %v", m3dTokenFast, m3dTokenGoog, len(st.Records), ex)
	}
	if len(st.Modes) != 0 {
		t.Fatalf("the From-gate's fixture DNS still holds name modes %v from an earlier test (SERVFAIL, silence, ...): a test that switches a name restores it when it ends, so something died without its cleanup", st.Modes)
	}
}

// requireSinkAliases checks, from the test container (the embedded DNS module 3 and its workers use too), that every network alias the From-gate tests name reaches the sink it is meant for, and then makes
// m3dHostCtl the host the adapter tests' accounts name for the controllable sink. ctlAddrs are the addresses the sink's own service name resolves to.
func (e *m3aEnv) requireSinkAliases(t *testing.T, ctlAddrs []string) {
	t.Helper()
	resolve := func(host string) []string {
		all, err := net.DefaultResolver.LookupHost(e.ctx, host)
		var addrs []string
		for _, a := range all {
			if ip := net.ParseIP(a); ip != nil && ip.To4() != nil { // the private network is IPv4 only: an address of another family is not the sink's
				addrs = append(addrs, a)
			}
		}
		if err != nil || len(addrs) == 0 {
			t.Fatalf("the network alias %q does not resolve to an IPv4 address from the test container (%v, answers %v): docker-compose.yml gives it to a sink on the private network (the From-gate tests name providers by their host strings)", host, err, all)
		}
		for _, a := range addrs {
			if !m3hPrivateIPv4(a) {
				t.Fatalf("the network alias %q resolves to %v, which is not a private address: module 3's destination guard would refuse it at connect time", host, addrs)
			}
		}
		return addrs
	}
	within := func(host string, got, want []string) {
		for _, a := range got {
			if !slices.Contains(want, a) {
				t.Fatalf("the network alias %q resolves to %v, which is not the sink it is meant for (%v)", host, got, want)
			}
		}
	}
	within(m3dHostCtl, resolve(m3dHostCtl), ctlAddrs)
	within(m3dHostGoog, resolve(m3dHostGoog), ctlAddrs)
	mail := resolve(m3dHostMail)
	within(m3dHostFast, resolve(m3dHostFast), mail)
	for _, a := range mail {
		if slices.Contains(ctlAddrs, a) {
			t.Fatalf("the Mailpit sink (%s) and the controllable sink share the address %s: they are two services", m3dHostMail, a)
		}
	}
	e.sinkIP = m3dHostCtl
}

func (e *m3aEnv) dnsSeq(t *testing.T) int {
	t.Helper()
	var st struct {
		Seq int `json:"seq"`
	}
	if err := json.Unmarshal(e.dnsCall(t, http.MethodGet, "/api/state", nil, http.StatusOK), &st); err != nil {
		t.Fatalf("the fixture's GET /api/state is not {seq}: %v", err)
	}
	return st.Seq
}

// dnsPut REPLACES the record set (name, type) of the fixture; no values deletes it.
func (e *m3aEnv) dnsPut(t *testing.T, name, typ string, values ...string) {
	t.Helper()
	if values == nil {
		values = []string{}
	}
	e.dnsCall(t, http.MethodPut, "/api/dns/records", map[string]any{"name": name, "type": typ, "values": values}, http.StatusOK)
}

// dnsMode switches a name (and everything below it) to servfail, silent, refused, nxdomain or normal, and puts it back to normal when the test ends, whatever happens to it.
func (e *m3aEnv) dnsMode(t *testing.T, name, mode string) {
	t.Helper()
	e.dnsCall(t, http.MethodPut, "/api/dns/modes", map[string]string{"name": name, "mode": mode}, http.StatusOK)
	if mode != "normal" {
		t.Cleanup(func() {
			status, out := e.raw.do(t, e.dnsURL, http.MethodPut, "/api/dns/modes", "", map[string]string{"name": name, "mode": "normal"})
			if status != http.StatusOK {
				t.Errorf("could not put the name %s of the From-gate's fixture DNS back to normal (HTTP %d: %s)", name, status, e.raw.snip(out))
			}
		})
	}
}

// dnsQueriesSince are the questions the fixture answered after the sequence number seq (read it with dnsSeq before the call under test).
func (e *m3aEnv) dnsQueriesSince(t *testing.T, seq int) []m3dQuery {
	t.Helper()
	var qs []m3dQuery
	if err := json.Unmarshal(e.dnsCall(t, http.MethodGet, "/api/dns/queries?since="+url.QueryEscape(fmt.Sprint(seq)), nil, http.StatusOK), &qs); err != nil {
		t.Fatalf("the fixture's GET /api/dns/queries is not a list of questions: %v", err)
	}
	return qs
}

// m3dKinds is what the fixture said to each question about (name, type), in order; nothing when the gate never asked.
func m3dKinds(qs []m3dQuery, name, typ string) []string {
	var out []string
	for _, q := range qs {
		if q.Name == name && q.Type == typ {
			out = append(out, q.Kind)
		}
	}
	return out
}

// m3dAsked lists the (type name) of every question, for a failure message.
func m3dAsked(qs []m3dQuery) string {
	seen := map[string]bool{}
	var out []string
	for _, q := range qs {
		k := q.Type + " " + q.Name + " -> " + q.Kind
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		return "(no question at all)"
	}
	return strings.Join(out, "; ")
}

// m3dRequireAsked fails unless the gate asked the fixture for (name, typ) and every answer it got is one of kinds: the proof that a verdict came from the DNS and not from a gate that was switched off.
func m3dRequireAsked(t *testing.T, what string, qs []m3dQuery, name, typ string, kinds ...string) {
	t.Helper()
	got := m3dKinds(qs, name, typ)
	if len(got) == 0 {
		t.Errorf("%s: module 3 never asked the fixture DNS for the %s of %s: its verdict did not come from DNS (asked: %s)", what, typ, name, m3dAsked(qs))
		return
	}
	for _, k := range got {
		if !slices.Contains(kinds, k) {
			t.Errorf("%s: the fixture answered the %s question about %s with %q, want one of %v (asked: %s)", what, typ, name, k, kinds, m3dAsked(qs))
			return
		}
	}
}

// m3dRequireNotAsked fails when the gate asked the fixture anything about the name (or a name below it).
func m3dRequireNotAsked(t *testing.T, what string, qs []m3dQuery, domain string) {
	t.Helper()
	for _, q := range qs {
		if q.Name == domain || strings.HasSuffix(q.Name, "."+domain) {
			t.Errorf("%s: module 3 asked the fixture DNS for the %s of %s although the gate was switched off (asked: %s)", what, q.Type, q.Name, m3dAsked(qs))
			return
		}
	}
}

// ---- module 3's own record of an account's From verdict (the superuser reads its table: ground truth beside the adapter) ----

type m3dRow struct {
	State       string
	Policy      string
	SPFAligned  bool
	Acked       bool   // an acknowledgment was recorded
	AckForThis  bool   // ... and it is for the verdict now in force (from_warning_acknowledged_for = from_verdict_fingerprint)
	GateMode    string // from_check_detail's from_gate_mode: "off" when the check was skipped by policy (disclosed as skipped, M3-P4-OD-12), "" when the gate ran
	CheckedAt   bool
	AttemptText bool // from_last_attempt_error holds text
}

func (e *m3aEnv) gateRow(t *testing.T, accountID string) m3dRow {
	t.Helper()
	var r m3dRow
	err := e.db.QueryRow(e.ctx, `SELECT from_verification_state::text, COALESCE(from_dmarc_policy::text, ''), COALESCE(from_spf_aligned, false),
			from_warning_acknowledged_at IS NOT NULL, COALESCE(from_warning_acknowledged_for = from_verdict_fingerprint, false),
			COALESCE(from_check_detail->>'from_gate_mode', ''), from_checked_at IS NOT NULL, from_last_attempt_error IS NOT NULL
		FROM send_byo.accounts WHERE id = $1::uuid`, accountID).Scan(&r.State, &r.Policy, &r.SPFAligned, &r.Acked, &r.AckForThis, &r.GateMode, &r.CheckedAt, &r.AttemptText)
	if err != nil {
		t.Fatalf("read module 3's own record of the From verdict of account %s: %v", accountID, err)
	}
	return r
}

// routable is the From-gate's term of module 3's routing predicate, written out from Docs/history/phase-4-from-gate.md section 8: `aligned`, or `unaligned_warned` with an acknowledgment for the verdict now in
// force. Nothing else may send (`blocked` and `unverified` never do: the list is positive). It is the gate's term only: credential verification, quarantine and the rest are other terms.
func (r m3dRow) routable() bool {
	return r.State == "aligned" || (r.State == "unaligned_warned" && r.Acked && r.AckForThis)
}

func (r m3dRow) String() string {
	return fmt.Sprintf("state %s, dmarc policy %q, spf aligned %v, acknowledged %v (for this verdict %v), checked %v, gate mode in detail %q, last attempt error recorded %v", r.State, r.Policy, r.SPFAligned, r.Acked, r.AckForThis, r.CheckedAt, r.GateMode, r.AttemptText)
}
