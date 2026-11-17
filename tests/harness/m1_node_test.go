//go:build harness && m1node

package harness

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	m1results "github.com/coldmail-platform/platform/internal/adapters/m1"
)

// Phase 3, chunk B7: module 1 with its REAL verifier node. The control plane under test is `m1n` (module 1 built from the pinned source, its SMTP stage wired to the real node, the
// node promoted by run.sh after its first green audit), the node is module 1's own image (truemail 3.3.1 plus the fork's wrapper, the destination guard ON), and the Internet they
// talk to is the fixture (tests/harness/fx: DNS, the "what is my IP" endpoint, the recipient's SMTP host) on a second internal network whose /24 the guard calls public. Nothing in a
// module is modified and the vendor (ZeroBounce) stays disabled. These tests run only behind the build tag m1node, in run.sh's scope m1node (stages M1N-1 to M1N-6); the plain
// `-tags harness` binary never contains them.
//
// WHAT IS PROVED.
//
//	TestM1Node_Preconditions        the stack is what the other tests assume: the fixture is loaded, module 1 is ready, the node's row is active, green and port25_open, a synchronous
//	                                verify shows a node verdict (meta.node_id, meta.verifier_ip), and the paired control: with the node row set to provisioning the SAME call answers
//	                                risky/plausible + no_eligible_node (failure_origin own) and the node is not called; restored, it is served by the node again
//	TestM1Node_BulkRecipeTable      ONE bulk job holds one address per producible recipe of the research's table (Docs/history/p3-research/m1-api-and-node.md, 2.8) and every row's (status,
//	                                reason, confidence, failure_origin, key signals, flags) is asserted EXACTLY: valid/accepted, valid/mailbox_full, invalid/{no_mx (NXDOMAIN and NODATA),
//	                                null_mx, mailbox_not_found, spam_trap}, risky/{catch_all, disposable, provider_no_smtp, non_public_mx}, unknown/{greylisted remote, dns_error remote
//	                                (SERVFAIL), own (silence), undetermined (REFUSED), smtp_timeout remote, smtp_error remote and undetermined, connection_failed undetermined, smtp_blocked
//	                                own}, and a duplicate spelling of the first address (one check, one verdict); the platform's own client (m1results.Drain) reads the same rows; the
//	                                fixture's session log shows exactly two RCPTs (target and nx- probe), from the node, for every recipe that reaches the node and none for the others;
//	                                its DNS log shows both module 1 and the node asking it
//	TestM1Node_CacheHit             the same address in a second job answers flags.from_cache=true with the original signals and no new SMTP session; the paired control (a non-cacheable
//	                                unknown) is verified again and reaches the node again
//	TestM1Node_Control*Family       the paired controls: ONE job per step holds the row whose fixture knob is changed and two bystanders; the knob of ONE name is flipped through the fixture's
//	                                control API (or, for the lists family, one row of module 1's own list tables), and exactly that row's verdict changes to the exactly expected one while the
//	                                bystanders stay what they were; the last step restores the first one. Families: the RCPT answer (valid, mailbox_not_found, mailbox_full, greylisted,
//	                                smtp_blocked, smtp_error), the catch-all answer, the transport (stall, dropped connection, nothing listening), DNS (SERVFAIL, REFUSED, silence, NXDOMAIN),
//	                                the lists (disposable, spam trap)
//
// NOT PRODUCIBLE WITH THIS WIRING, so not planned (the charge table keeps their rows proven by the existing oracle): smtp_unwired (it needs the stage unwired: a restart or a second module 1
// without a node), warmup_cap (it needs TM_WARMUP_DAILY_CAP above 0: a second profile), the M1-16 "risky/plausible with no token" (the node always answers with a catch-all block, so the not-probed
// branch cannot be reached), unknown_error (a decrypt, policy or compute failure repeated five times), audit_red (a red audit is a different stack: the watcher loop is off by design), external_verified
// (the vendor stays disabled), and rate_limited (it needs 31 session starts at one domain inside a minute and the bucket's edge has not been read to its last line, [U9] of the research; a job that
// waits on it settles the honest verdict only after TM_QUEUE_MAX_RETRY_AGE_SECONDS). no_eligible_node IS produced, once, by the Preconditions control.

// m1nRecipe is one row of the table: the address, what module 1 must make of it, and whether the node is called for it.
type m1nRecipe struct {
	id      string
	local   string
	domain  string
	want    m1nWant
	reaches bool   // the node opens a session at the fixture: two RCPTs (the target and the nx- probe)
	dnsKind string // "" or the kind the fixture's DNS log must show for the domain (servfail, refused, silent)
}

func (e *m1nEnv) recipes() []m1nRecipe {
	return []m1nRecipe{
		{id: "valid", local: "ada", domain: "valid.fixture.test", want: e.wantValid(), reaches: true},
		{id: "mailbox_full", local: "bob", domain: "full.fixture.test", want: e.wantFull(), reaches: true},
		{id: "no_mx", local: "cyd", domain: "nomx.fixture.test", want: wantNoMX()},
		{id: "no_mx_nodata", local: "cyd2", domain: "nodata.fixture.test", want: wantNoMX()},
		{id: "null_mx", local: "dee", domain: "nullmx.fixture.test", want: wantNullMX()},
		{id: "mailbox_not_found", local: "eli", domain: "gone.fixture.test", want: e.wantGone(), reaches: true},
		{id: "spam_trap", local: "fay", domain: "trap.fixture.test", want: wantTrap()},
		{id: "catch_all", local: "gus", domain: "catchall.fixture.test", want: e.wantCatchAll(), reaches: true},
		{id: "disposable", local: "hal", domain: "disposable.fixture.test", want: wantDisposable()},
		{id: "provider_no_smtp", local: "ian", domain: "provider.fixture.test", want: e.wantProvider()},
		{id: "non_public_mx", local: "jan", domain: "nonpublic.fixture.test", want: e.wantNonPublic()},
		{id: "greylisted", local: "kim", domain: "grey.fixture.test", want: e.wantGrey(), reaches: true},
		{id: "dns_servfail_remote", local: "lee", domain: "servfail.fixture.test", want: wantServfail(), dnsKind: "servfail"},
		{id: "dns_silent_own", local: "max", domain: "silent.fixture.test", want: wantSilent(), dnsKind: "silent"},
		{id: "dns_refused_undetermined", local: "ned", domain: "refused.fixture.test", want: wantRefused(), dnsKind: "refused"},
		{id: "smtp_timeout_remote", local: "oli", domain: "stall.fixture.test", want: e.wantStall(), reaches: true},
		{id: "smtp_error_remote", local: "pam", domain: "ambiguous.fixture.test", want: e.wantAmbiguous(), reaches: true},
		{id: "smtp_error_undetermined", local: "quin", domain: "dropped.fixture.test", want: e.wantDropped(), reaches: true},
		{id: "connection_failed_undetermined", local: "rae", domain: "noconn.fixture.test", want: e.wantNoConn()},
		{id: "smtp_blocked_own", local: "sam", domain: "blocked.fixture.test", want: e.wantBlocked(), reaches: true},
	}
}

// TestM1Node_Preconditions: see the file comment.
func TestM1Node_Preconditions(t *testing.T) {
	e := newM1nEnv(t)

	// the fixture is loaded with the whole recipe table
	var st struct {
		Zones       []string       `json:"zones"`
		DNSRecords  int            `json:"dns_records"`
		DNSModes    map[string]any `json:"dns_modes"`
		SMTPDomains int            `json:"smtp_domains"`
	}
	e.fx.do("GET", "/api/state", nil, &st)
	t.Logf("fixture: zones %v, %d DNS names, DNS modes %v, %d SMTP domains", st.Zones, st.DNSRecords, st.DNSModes, st.SMTPDomains)
	if st.SMTPDomains < 17 || st.DNSRecords < 30 || len(st.DNSModes) < 3 {
		t.Fatalf("the fixture holds %d SMTP domains, %d DNS names and %d DNS modes: the recipe table is not all there (tests/harness/m1node/fixture.json)", st.SMTPDomains, st.DNSRecords, len(st.DNSModes))
	}

	// module 1 is ready, and says what it was configured with (the config string prints smtp=enabled from configuration, not from the wired state: the verdicts below are the proof)
	var rz struct {
		Ready  bool            `json:"ready"`
		Status string          `json:"status"`
		Config string          `json:"config"`
		Checks map[string]bool `json:"checks"`
	}
	if code := e.getJSON("/readyz", &rz); code != 200 || !rz.Ready {
		t.Fatalf("module 1's /readyz answered HTTP %d, ready=%t", code, rz.Ready)
	}
	t.Logf("module 1 /readyz: status %s, checks %v, config %q", rz.Status, rz.Checks, rz.Config)

	// the node's row, as module 1's own database holds it
	admin := dial(t, requireEnv(t, envAdminURL), nil)
	var status, audit string
	var port25 bool
	if err := admin.QueryRow(e.ctx, `select status, coalesce(audit_status, ''), coalesce(port25_open, false) from verify.nodes where id = $1`, e.nodeID).Scan(&status, &audit, &port25); err != nil {
		t.Fatalf("reading the node's row verify.nodes[%s]: %v", e.nodeID, err)
	}
	t.Logf("node row: id=%s status=%s audit_status=%s port25_open=%t", e.nodeID, status, audit, port25)
	if status != "active" || audit != "green" || !port25 {
		t.Fatalf("the node's row is status=%s audit_status=%s port25_open=%t, want active, green, true (run.sh promotes it after the first green audit row)", status, audit, port25)
	}
	restore := func() {
		if _, err := admin.Exec(context.Background(), `update verify.nodes set status = 'active', port25_open = true where id = $1`, e.nodeID); err != nil {
			t.Errorf("restoring the node's row failed: %v (the tests after this one need an active node)", err)
		}
	}

	// a synchronous verify shows a node verdict
	before := e.fx.seq()
	addr := fresh("pre") + "@valid.fixture.test"
	v := e.syncVerify(addr)
	t.Logf("sync %s -> %s/%s conf=%d node_id=%v verifier_ip=%v signals=%v", addr, v.Status, v.Reason, v.Confidence, deref(v.Meta.NodeID), deref(v.Meta.VerifierIP), v.Signals)
	if v.Status != "valid" || v.Reason != "accepted" || deref(v.Meta.NodeID) != e.nodeID || deref(v.Meta.VerifierIP) != e.nodeIP {
		t.Fatalf("the synchronous verdict is %s/%s with node_id %q and verifier_ip %q, want valid/accepted served by %s at %s", v.Status, v.Reason, deref(v.Meta.NodeID), deref(v.Meta.VerifierIP), e.nodeID, e.nodeIP)
	}
	if p := e.checkSessions(e.fx.sessions(before, "valid.fixture.test"), "valid.fixture.test", strings.Split(addr, "@")[0], true); len(p) != 0 {
		t.Fatalf("the fixture's view of the synchronous verify: %s", strings.Join(p, "; "))
	}

	// the paired control: the node row is not active, so the same call must NOT be served by the node, at once, and the failure is ours (own)
	if _, err := admin.Exec(e.ctx, `update verify.nodes set status = 'provisioning' where id = $1`, e.nodeID); err != nil {
		t.Fatalf("setting the node to provisioning: %v", err)
	}
	t.Cleanup(restore)
	before = e.fx.seq()
	addr2 := fresh("pre") + "@valid.fixture.test"
	v2 := e.syncVerify(addr2)
	t.Logf("sync %s with the node provisioning -> %s/%s conf=%d node_id=%v signals=%v", addr2, v2.Status, v2.Reason, v2.Confidence, deref(v2.Meta.NodeID), v2.Signals)
	if v2.Status != "risky" || v2.Reason != "plausible" || v2.Signals["smtp_skipped_reason"] != "no_eligible_node" || v2.Signals["failure_origin"] != "own" || v2.Meta.NodeID != nil && *v2.Meta.NodeID != "" {
		t.Fatalf("with the node provisioning the verdict must be risky/plausible + smtp_skipped_reason no_eligible_node + failure_origin own and carry no node, got %s/%s signals %v node_id %v", v2.Status, v2.Reason, v2.Signals, deref(v2.Meta.NodeID))
	}
	if n := len(e.fx.sessions(before, "")); n != 0 {
		t.Fatalf("the fixture saw %d RCPT(s) while the node was not eligible, want none", n)
	}
	restore()
	addr3 := fresh("pre") + "@valid.fixture.test"
	if v3 := e.syncVerify(addr3); v3.Status != "valid" || deref(v3.Meta.NodeID) != e.nodeID {
		t.Fatalf("after the node row is active again the verdict is %s/%s node_id %q, want valid/accepted served by the node", v3.Status, v3.Reason, deref(v3.Meta.NodeID))
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// TestM1Node_BulkRecipeTable: see the file comment.
func TestM1Node_BulkRecipeTable(t *testing.T) {
	e := newM1nEnv(t)
	recipes := e.recipes()
	var items []m1results.Item
	for i, r := range recipes {
		items = append(items, m1results.Item{ItemID: fmt.Sprintf("r%02d-%s", i, r.id), Address: r.local + "@" + r.domain})
	}
	dupID := fmt.Sprintf("r%02d-duplicate_of_valid", len(items))
	items = append(items, m1results.Item{ItemID: dupID, Address: strings.ToUpper(recipes[0].local + "@" + recipes[0].domain)})
	baseline := e.fx.seq()
	started := time.Now()

	jobID, counts, rows := e.runJob(items)
	t.Logf("job %s: %d row(s) settled in %v; the 202's counts %+v", jobID, len(items), time.Since(started).Round(100*time.Millisecond), counts)
	if want := (m1results.SubmitCounts{Total: len(items), Enqueued: len(recipes), Duplicate: 1, Malformed: 0}); counts != want {
		t.Errorf("the 202's counts are %+v, want %+v (the duplicate spelling is one check)", counts, want)
	}

	t.Logf("the recipe table as module 1 reported it (ordinal, recipe, verdict):")
	for i, r := range recipes {
		row := rows[items[i].ItemID]
		t.Logf("  %2d %-32s %-26s %s", i, r.id, r.local+"@"+r.domain, row.short())
	}
	dup := rows[dupID]
	dupOf := "none"
	if dup.DuplicateOf != nil {
		dupOf = fmt.Sprint(*dup.DuplicateOf) // the ordinal, not the pointer
	}
	t.Logf("  %2d %-32s %-26s %s duplicate_of=%s", len(recipes), "duplicate_of_valid", strings.ToUpper(recipes[0].local+"@"+recipes[0].domain), dup.short(), dupOf)

	var bad []string
	for i, r := range recipes {
		row := rows[items[i].ItemID]
		for _, p := range r.want.check(row) {
			bad = append(bad, fmt.Sprintf("%s (%s@%s): %s", r.id, r.local, r.domain, p))
		}
		if row.DuplicateOf != nil {
			bad = append(bad, fmt.Sprintf("%s: duplicate_of %d on a first row", r.id, *row.DuplicateOf))
		}
	}
	if dup.DuplicateOf == nil || *dup.DuplicateOf != 0 {
		bad = append(bad, fmt.Sprintf("the duplicate spelling is duplicate_of %v, want ordinal 0 (the first row of the address)", dup.DuplicateOf))
	}
	if first := rows[items[0].ItemID]; dup.Status != first.Status || dup.Reason != first.Reason || dup.Confidence != first.Confidence {
		bad = append(bad, fmt.Sprintf("the duplicate got %s/%s conf %d, the first spelling %s/%s conf %d: one check, one verdict", dup.Status, dup.Reason, dup.Confidence, first.Status, first.Reason, first.Confidence))
	}

	// the fixture's own view: which recipes reached the node, and from where
	sessions := e.fx.sessions(baseline, "")
	t.Logf("the SMTP fixture saw %d RCPT(s) in this job:", len(sessions))
	for _, s := range sessions {
		t.Logf("  session %d from %s: RCPT %s -> rule %q, %s %s", s.Session, s.Remote, s.Rcpt, s.Rule, s.Action, s.Reply)
	}
	for _, r := range recipes {
		bad = append(bad, e.checkSessions(sessions, r.domain, r.local, r.reaches)...)
	}
	if len(sessions) != 2*countReaching(recipes) {
		bad = append(bad, fmt.Sprintf("the fixture saw %d RCPT(s) in total, want %d (two per recipe that reaches the node)", len(sessions), 2*countReaching(recipes)))
	}
	queries := e.fx.dnsQueries(baseline)
	var fromNode, fromOthers int
	kinds := map[string]map[string]bool{}
	for _, q := range queries {
		if q.Remote == e.nodeIP {
			fromNode++
		} else {
			fromOthers++
		}
		if kinds[q.Name] == nil {
			kinds[q.Name] = map[string]bool{}
		}
		kinds[q.Name][q.Kind] = true
	}
	t.Logf("the DNS fixture answered %d query(ies) in this job: %d from the node, %d from module 1 (and the fixture network's other clients)", len(queries), fromNode, fromOthers)
	if fromNode == 0 || fromOthers == 0 {
		bad = append(bad, fmt.Sprintf("the DNS fixture was asked %d time(s) by the node and %d time(s) by module 1: both must resolve through it", fromNode, fromOthers))
	}
	for _, r := range recipes {
		if r.dnsKind != "" && !kinds[r.domain][r.dnsKind] {
			bad = append(bad, fmt.Sprintf("%s: the DNS fixture never answered %s with %q (kinds seen: %v): module 1 did not ask it", r.id, r.domain, r.dnsKind, kinds[r.domain]))
		}
	}

	// the platform's own client reads the same rows (the live-client cross-check: m1results.Drain is what the adapter will use)
	var drained []m1results.Row
	dctx, cancel := context.WithTimeout(e.ctx, time.Minute)
	defer cancel()
	if _, err := e.m1.Drain(dctx, jobID, func(page []m1results.Row) error { drained = append(drained, page...); return nil }); err != nil {
		bad = append(bad, fmt.Sprintf("m1results.Drain of the finished job failed: %v", err))
	}
	if len(drained) != len(items) {
		bad = append(bad, fmt.Sprintf("m1results.Drain delivered %d row(s) for %d input rows", len(drained), len(items)))
	}
	for _, d := range drained {
		raw := rows[d.ItemID]
		origin, _ := raw.Signals["failure_origin"].(string)
		skip, _ := raw.Signals["smtp_skipped_reason"].(string)
		if d.Status != raw.Status || d.Reason != raw.Reason || d.FailureOrigin != origin || d.SkipReason != skip || d.FromCache != raw.Flags["from_cache"] {
			bad = append(bad, fmt.Sprintf("m1results.Row of %s is %s/%s origin %q skip %q cache %t, the raw row says %s/%s origin %q skip %q cache %t", d.ItemID, d.Status, d.Reason, d.FailureOrigin, d.SkipReason, d.FromCache, raw.Status, raw.Reason, origin, skip, raw.Flags["from_cache"]))
		}
	}

	if len(bad) != 0 {
		t.Fatalf("%d difference(s) between the recipe table and module 1's report:\n  %s", len(bad), strings.Join(bad, "\n  "))
	}
	t.Logf("every one of the %d recipe rows, and the duplicate, is exactly what the table says; %d session(s) at the fixture, all from the node", len(recipes), len(sessions))
}

func countReaching(rs []m1nRecipe) int {
	n := 0
	for _, r := range rs {
		if r.reaches {
			n++
		}
	}
	return n
}

// TestM1Node_CacheHit: see the file comment.
func TestM1Node_CacheHit(t *testing.T) {
	e := newM1nEnv(t)
	hit := fresh("cache") + "@valid.fixture.test"
	miss := fresh("cache") + "@grey.fixture.test"
	item := func(id, addr string) []m1results.Item { return []m1results.Item{{ItemID: id, Address: addr}} }

	// job 1: both are checked for real
	b1 := e.fx.seq()
	_, _, r1 := e.runJob(item("a-hit", hit))
	_, _, r2 := e.runJob(item("a-miss", miss))
	first, firstMiss := r1["a-hit"], r2["a-miss"]
	t.Logf("first job: %s -> %s; %s -> %s", hit, first.short(), miss, firstMiss.short())
	var bad []string
	bad = append(bad, prefixed("first valid", e.wantValid().check(first))...)
	bad = append(bad, prefixed("first greylisted", e.wantGrey().check(firstMiss))...)
	s1 := e.fx.sessions(b1, "")
	bad = append(bad, e.checkSessions(s1, "valid.fixture.test", strings.Split(hit, "@")[0], true)...)
	bad = append(bad, e.checkSessions(s1, "grey.fixture.test", strings.Split(miss, "@")[0], true)...)

	// job 2: the same addresses again, one row each, different item ids
	b2 := e.fx.seq()
	_, _, q1 := e.runJob(item("b-hit", hit))
	_, _, q2 := e.runJob(item("b-miss", miss))
	second, secondMiss := q1["b-hit"], q2["b-miss"]
	t.Logf("second job: %s -> %s from_cache=%t; %s -> %s from_cache=%t", hit, second.short(), second.Flags["from_cache"], miss, secondMiss.short(), secondMiss.Flags["from_cache"])
	if !second.Flags["from_cache"] {
		bad = append(bad, "the second check of a settled valid address is not flags.from_cache=true")
	}
	if second.Status != first.Status || second.Reason != first.Reason || second.Confidence != first.Confidence {
		bad = append(bad, fmt.Sprintf("the cached row is %s/%s conf %d, the first answer was %s/%s conf %d", second.Status, second.Reason, second.Confidence, first.Status, first.Reason, first.Confidence))
	}
	for _, k := range []string{"catchall_conclusion", "engine_mx_hosts", "brain_mx_provider"} {
		if fmt.Sprint(second.Signals[k]) != fmt.Sprint(first.Signals[k]) || second.Signals[k] == nil {
			bad = append(bad, fmt.Sprintf("the cached row's signals.%s is %v, the original's %v: a hit keeps the original stored signals", k, second.Signals[k], first.Signals[k]))
		}
	}
	if n := len(e.fx.sessions(b2, "valid.fixture.test")); n != 0 {
		bad = append(bad, fmt.Sprintf("the cache hit still reached the node: %d RCPT(s) at valid.fixture.test since the first job", n))
	}
	// the control: an unknown is never cached, so it is verified again, reaches the node again, and is not from_cache
	if secondMiss.Flags["from_cache"] {
		bad = append(bad, "the second check of a greylisted address is from_cache=true: an unknown must never be cached")
	}
	bad = append(bad, prefixed("second greylisted", e.wantGrey().check(secondMiss))...)
	bad = append(bad, e.checkSessions(e.fx.sessions(b2, "grey.fixture.test"), "grey.fixture.test", strings.Split(miss, "@")[0], true)...)

	// the platform's own client sees the cache flag the way the charge table reads it (flags.from_cache, never meta.from_cache)
	var drained []m1results.Row
	jobID, _, _ := e.runJob(item("c-hit", hit))
	dctx, cancel := context.WithTimeout(e.ctx, time.Minute)
	defer cancel()
	if _, err := e.m1.Drain(dctx, jobID, func(page []m1results.Row) error { drained = append(drained, page...); return nil }); err != nil || len(drained) != 1 || !drained[0].FromCache {
		bad = append(bad, fmt.Sprintf("m1results.Drain of a third job for the same address: err %v, rows %+v, want one row with FromCache true", err, drained))
	}
	if len(bad) != 0 {
		t.Fatalf("%d difference(s):\n  %s", len(bad), strings.Join(bad, "\n  "))
	}
}

func prefixed(prefix string, ps []string) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = prefix + ": " + p
	}
	return out
}

// ---- the paired controls ---------------------------------------------------------------------------------------------------------------------------------

// m1nStep is one job of a control: apply changes the fixture knob of ONE name, the row at that name must then be exactly want, and the two bystanders (an ordinary accepted address at
// valid.fixture.test, and a name that does not exist) must be exactly what they always are.
type m1nStep struct {
	name    string
	apply   func()
	domain  string
	want    m1nWant
	reaches bool // the node opens a session for the changed row
}

// runFamily runs the steps in order. Each step is its own bulk job with fresh addresses (the cache is keyed by address), so a verdict can only change because the knob did.
func (e *m1nEnv) runFamily(t *testing.T, family string, steps []m1nStep) {
	t.Helper()
	var prev string
	for i, s := range steps {
		cur := s.want.status + "/" + s.want.reason + "/" + s.want.origin
		if cur == prev {
			t.Fatalf("%s step %d (%s) wants the same verdict as the step before: a control that changes nothing proves nothing", family, i, s.name)
		}
		prev = cur
		s.apply()
		local, byValid, byNoMX := fresh("c"), fresh("by"), fresh("by")
		baseline := e.fx.seq()
		_, _, rows := e.runJob([]m1results.Item{
			{ItemID: "changed", Address: local + "@" + s.domain},
			{ItemID: "bystander-valid", Address: byValid + "@valid.fixture.test"},
			{ItemID: "bystander-nomx", Address: byNoMX + "@nomx.fixture.test"},
		})
		t.Logf("%s step %d %-34s %-24s -> %s | bystanders: %s ; %s", family, i, s.name, s.domain, rows["changed"].short(), rows["bystander-valid"].short(), rows["bystander-nomx"].short())
		var bad []string
		bad = append(bad, prefixed("the changed row", s.want.check(rows["changed"]))...)
		bad = append(bad, prefixed("bystander valid.fixture.test", e.wantValid().check(rows["bystander-valid"]))...)
		bad = append(bad, prefixed("bystander nomx.fixture.test", wantNoMX().check(rows["bystander-nomx"]))...)
		sessions := e.fx.sessions(baseline, "")
		bad = append(bad, e.checkSessions(sessions, s.domain, local, s.reaches)...)
		bad = append(bad, e.checkSessions(sessions, "valid.fixture.test", byValid, true)...)
		if len(bad) != 0 {
			t.Fatalf("%s step %d (%s): %d difference(s):\n  %s", family, i, s.name, len(bad), strings.Join(bad, "\n  "))
		}
	}
}

// TestM1Node_ControlRCPTFamily: the recipient server's answer at RCPT TO, changed for ONE domain: accepted, no such user, mailbox full, greylisted, blocked, ambiguous, and back.
func TestM1Node_ControlRCPTFamily(t *testing.T) {
	e := newM1nEnv(t)
	const dom = "ctl-rcpt.fixture.test"
	t.Cleanup(func() { e.fx.setRules(dom, m1nBaseRules()...) })
	set := func(rules ...m1nRule) func() { return func() { e.fx.setRules(dom, rules...) } }
	e.runFamily(t, "rcpt", []m1nStep{
		{name: "accepted (the baseline)", apply: set(m1nBaseRules()...), domain: dom, want: e.wantValid(), reaches: true},
		{name: "550 5.1.1 no such user", apply: set(m1nRule{Match: "*", Reply: m1nUnknownUser}), domain: dom, want: e.wantGone(), reaches: true},
		{name: "552 5.2.2 mailbox full", apply: set(m1nRule{Match: "nx-*", Reply: m1nUnknownUser}, m1nRule{Match: "*", Reply: "552 5.2.2 Mailbox full"}), domain: dom, want: e.wantFull(), reaches: true},
		{name: "450 4.2.0 greylisted", apply: set(m1nRule{Match: "*", Reply: "450 4.2.0 Greylisted, please try again later"}), domain: dom, want: e.wantGrey(), reaches: true},
		{name: "554 5.7.1 blocked by policy", apply: set(m1nRule{Match: "*", Reply: "554 5.7.1 Client host blocked by policy"}), domain: dom, want: e.wantBlocked(), reaches: true},
		{name: "550 5.7.0 message rejected", apply: set(m1nRule{Match: "*", Reply: "550 5.7.0 Message rejected"}), domain: dom, want: e.wantAmbiguous(), reaches: true},
		{name: "accepted again (restored)", apply: set(m1nBaseRules()...), domain: dom, want: e.wantValid(), reaches: true},
	})
}

// TestM1Node_ControlCatchAllFamily: the only difference between valid/accepted and risky/catch_all is whether the probe address is refused.
func TestM1Node_ControlCatchAllFamily(t *testing.T) {
	e := newM1nEnv(t)
	const dom = "ctl-catch.fixture.test"
	t.Cleanup(func() { e.fx.setRules(dom, m1nBaseRules()...) })
	e.runFamily(t, "catch-all", []m1nStep{
		{name: "the probe is refused (baseline)", apply: func() { e.fx.setRules(dom, m1nBaseRules()...) }, domain: dom, want: e.wantValid(), reaches: true},
		{name: "everything is accepted", apply: func() { e.fx.setRules(dom, m1nRule{Match: "*", Reply: m1nAccept}) }, domain: dom, want: e.wantCatchAll(), reaches: true},
		{name: "the probe is refused again", apply: func() { e.fx.setRules(dom, m1nBaseRules()...) }, domain: dom, want: e.wantValid(), reaches: true},
	})
}

// TestM1Node_ControlTransportFamily: what the node meets on the wire for ONE domain: a recipient that stalls, a connection closed after RCPT, and an MX host where nothing listens.
func TestM1Node_ControlTransportFamily(t *testing.T) {
	e := newM1nEnv(t)
	const trans, conn = "ctl-trans.fixture.test", "ctl-conn.fixture.test"
	mx := "mx." + conn
	t.Cleanup(func() { e.fx.setRules(trans, m1nBaseRules()...); e.fx.setA(mx, e.smtpIP) })
	e.runFamily(t, "transport", []m1nStep{
		{name: "answers (baseline)", apply: func() { e.fx.setRules(trans, m1nBaseRules()...) }, domain: trans, want: e.wantValid(), reaches: true},
		{name: "the target stalls (6 s)", apply: func() {
			e.fx.setRules(trans, m1nRule{Match: "nx-*", Reply: m1nUnknownUser}, m1nRule{Match: "*", Action: "stall", StallMs: 6000})
		}, domain: trans, want: e.wantStall(), reaches: true},
		{name: "the connection is dropped after RCPT", apply: func() {
			e.fx.setRules(trans, m1nRule{Match: "nx-*", Reply: m1nUnknownUser}, m1nRule{Match: "*", Action: "drop"})
		}, domain: trans, want: e.wantDropped(), reaches: true},
		{name: "answers again (restored)", apply: func() { e.fx.setRules(trans, m1nBaseRules()...) }, domain: trans, want: e.wantValid(), reaches: true},
		{name: "the MX host moves to a closed port", apply: func() { e.fx.setA(mx, e.nodeIP) }, domain: conn, want: e.wantNoConn(), reaches: false},
		{name: "the MX host is back (restored)", apply: func() { e.fx.setA(mx, e.smtpIP) }, domain: conn, want: e.wantValid(), reaches: true},
	})
}

// TestM1Node_ControlDNSFamily: what module 1's own resolver meets for ONE name: SERVFAIL (the recipient's side, charged), REFUSED (undetermined), silence (ours), NXDOMAIN, and back.
func TestM1Node_ControlDNSFamily(t *testing.T) {
	e := newM1nEnv(t)
	const dom = "ctl-dns.fixture.test"
	t.Cleanup(func() { e.fx.setMode(dom, "normal") })
	mode := func(m string) func() { return func() { e.fx.setMode(dom, m) } }
	e.runFamily(t, "dns", []m1nStep{
		{name: "answers (baseline)", apply: mode("normal"), domain: dom, want: e.wantValid(), reaches: true},
		{name: "SERVFAIL", apply: mode("servfail"), domain: dom, want: wantServfail()},
		{name: "REFUSED", apply: mode("refused"), domain: dom, want: wantRefused()},
		{name: "silence (no reply at all)", apply: mode("silent"), domain: dom, want: wantSilent()},
		{name: "NXDOMAIN", apply: mode("nxdomain"), domain: dom, want: wantNoMX()},
		{name: "answers again (restored)", apply: mode("normal"), domain: dom, want: e.wantValid(), reaches: true},
	})
}

// TestM1Node_ControlListsFamily: the cheap layers that run before any DNS answer matters are module 1's own list tables; one row of one table changes ONE domain's verdict, and the node is
// not called for it.
func TestM1Node_ControlListsFamily(t *testing.T) {
	e := newM1nEnv(t)
	const dom = "ctl-list.fixture.test"
	admin := dial(t, requireEnv(t, envAdminURL), nil)
	exec := func(sql string) func() {
		return func() {
			if _, err := admin.Exec(e.ctx, sql, dom); err != nil {
				t.Fatalf("%s: %v", sql, err)
			}
		}
	}
	insDisposable := exec(`insert into verify.disposable_domains (domain, source, added_at) values ($1, 'harness m1node control', now()) on conflict do nothing`)
	delDisposable := exec(`delete from verify.disposable_domains where domain = $1`)
	insTrap := exec(`insert into verify.spam_trap_domains (domain, source, added_at) values ($1, 'harness m1node control', now()) on conflict do nothing`)
	delTrap := exec(`delete from verify.spam_trap_domains where domain = $1`)
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `delete from verify.disposable_domains where domain = $1`, dom)
		_, _ = admin.Exec(context.Background(), `delete from verify.spam_trap_domains where domain = $1`, dom)
	})
	e.runFamily(t, "lists", []m1nStep{
		{name: "on no list (baseline)", apply: func() {}, domain: dom, want: e.wantValid(), reaches: true},
		{name: "on the disposable list", apply: insDisposable, domain: dom, want: wantDisposable()},
		{name: "off the list again", apply: delDisposable, domain: dom, want: e.wantValid(), reaches: true},
		{name: "on the spam-trap list", apply: insTrap, domain: dom, want: wantTrap()},
		{name: "off the list again", apply: delTrap, domain: dom, want: e.wantValid(), reaches: true},
	})
}

// ---- the instrument's own control --------------------------------------------------------------------------------------------------------------------------

// TestM1Node_ExpectationsBite is the control of every other test of this file: it needs no stack, and it proves that the comparison that decides them goes red, naming the field, for each
// way a row can be wrong (the verdict, the confidence, the failure origin present, missing or wrong, a signal missing, wrong or of the wrong type, a signal that must be absent, a flag, a
// cache flag nobody asked for), and that the session check goes red for a foreign source, a missing probe and a session where none may be.
func TestM1Node_ExpectationsBite(t *testing.T) {
	e := &m1nEnv{nodeIP: "198.51.100.3", smtpIP: "198.51.100.2"}
	want := e.wantGrey()
	good := m1nRow{Status: "unknown", Reason: "greylisted", Confidence: 10, Flags: map[string]bool{"from_cache": false},
		Signals: map[string]any{"failure_origin": "remote", "smtp_code": "450", "smtp_enhanced_code": "4.2.0", "greylisted": true, "retry_eligible": true, "retry_after_hint_s": float64(300), "engine_mx_hosts": []any{"198.51.100.2"}}}
	if p := want.check(good); len(p) != 0 {
		t.Fatalf("the control: a row that is exactly the want must pass, got %v", p)
	}
	clone := func(mut func(r *m1nRow)) m1nRow {
		r := good
		r.Flags = map[string]bool{"from_cache": false}
		r.Signals = map[string]any{}
		for k, v := range good.Signals {
			r.Signals[k] = v
		}
		mut(&r)
		return r
	}
	cases := []struct {
		name string
		mut  func(r *m1nRow)
		want string // a fragment the problem must contain
	}{
		{"another status", func(r *m1nRow) { r.Status = "risky" }, "verdict risky/greylisted"},
		{"another reason", func(r *m1nRow) { r.Reason = "smtp_error" }, "verdict unknown/smtp_error"},
		{"another confidence", func(r *m1nRow) { r.Confidence = 40 }, "confidence 40"},
		{"the origin missing", func(r *m1nRow) { delete(r.Signals, "failure_origin") }, "failure_origin is absent"},
		{"the origin wrong", func(r *m1nRow) { r.Signals["failure_origin"] = "own" }, `failure_origin is own, want "remote"`},
		{"a signal missing", func(r *m1nRow) { delete(r.Signals, "smtp_code") }, "signals.smtp_code is absent"},
		{"a signal wrong", func(r *m1nRow) { r.Signals["smtp_code"] = "451" }, "signals.smtp_code is 451"},
		{"a number as a string", func(r *m1nRow) { r.Signals["retry_after_hint_s"] = "300" }, "signals.retry_after_hint_s is 300"},
		{"a list that differs", func(r *m1nRow) { r.Signals["engine_mx_hosts"] = []any{"198.51.100.9"} }, "signals.engine_mx_hosts"},
		{"a flag cache nobody asked for", func(r *m1nRow) { r.Flags["from_cache"] = true }, "flags.from_cache is true"},
	}
	for _, c := range cases {
		p := want.check(clone(c.mut))
		if len(p) == 0 || !strings.Contains(strings.Join(p, "; "), c.want) {
			t.Fatalf("%s: the comparison said %v, want a problem containing %q", c.name, p, c.want)
		}
	}
	// a signal that must be absent, and an origin that must be absent
	valid := e.wantValid()
	vgood := m1nRow{Status: "valid", Reason: "accepted", Confidence: 90, Flags: map[string]bool{}, Signals: map[string]any{"catchall_conclusion": "not_catchall", "engine_mx_hosts": []any{"198.51.100.2"}, "brain_mx_provider": "self"}}
	if p := valid.check(vgood); len(p) != 0 {
		t.Fatalf("the control: the valid row must pass, got %v", p)
	}
	vgood.Signals["smtp_code"] = "250"
	if p := valid.check(vgood); len(p) == 0 || !strings.Contains(strings.Join(p, ";"), "signals.smtp_code is present") {
		t.Fatalf("a signal that must be absent was accepted: %v", p)
	}
	delete(vgood.Signals, "smtp_code")
	vgood.Signals["failure_origin"] = "own"
	if p := valid.check(vgood); len(p) == 0 || !strings.Contains(strings.Join(p, ";"), "want the key absent") {
		t.Fatalf("a failure_origin on a row that must not carry one was accepted: %v", p)
	}
	delete(vgood.Signals, "failure_origin")
	if p := wantDisposable().check(m1nRow{Status: "risky", Reason: "disposable", Confidence: 40, Flags: map[string]bool{"is_disposable": false}, Signals: map[string]any{}}); len(p) == 0 || !strings.Contains(strings.Join(p, ";"), "flags.is_disposable is false, want true") {
		t.Fatalf("a wrong flag was accepted: %v", p)
	}

	// the session check: exactly the target and the nx- probe, from the node, for a recipe that reaches it; nothing for one that does not
	sess := func(remote, rcpt string) m1nSession {
		return m1nSession{Remote: remote, Rcpt: rcpt, Domain: "valid.fixture.test"}
	}
	ok := []m1nSession{sess("198.51.100.3", "ada@valid.fixture.test"), sess("198.51.100.3", "nx-0123@valid.fixture.test")}
	if p := e.checkSessions(ok, "valid.fixture.test", "ada", true); len(p) != 0 {
		t.Fatalf("the control: target and probe from the node must pass, got %v", p)
	}
	bites := []struct {
		name     string
		sessions []m1nSession
		reaches  bool
		want     string
	}{
		{"a foreign source", []m1nSession{sess("198.51.100.9", "ada@valid.fixture.test"), ok[1]}, true, "only the node"},
		{"the probe missing", ok[:1], true, "want exactly the target and the probe"},
		{"a third RCPT", append(append([]m1nSession{}, ok...), sess("198.51.100.3", "ada@valid.fixture.test")), true, "3 RCPT(s) seen"},
		{"a session where none may be", ok, false, "must leave none"},
		{"no session where one is needed", nil, true, "0 RCPT(s) seen"},
	}
	for _, b := range bites {
		if p := e.checkSessions(b.sessions, "valid.fixture.test", "ada", b.reaches); len(p) == 0 || !strings.Contains(strings.Join(p, ";"), b.want) {
			t.Fatalf("%s: the session check said %v, want a problem containing %q", b.name, p, b.want)
		}
	}
}
