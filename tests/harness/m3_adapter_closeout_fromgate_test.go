//go:build harness

package harness

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/adapters/m3"
	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// TestM3Adapter_FromGatePassWarnBlock (Phase 3 close-out D2): module 3's From-gate ON (from_gate_mode=enforce, from_dns_resolver=m3dns:53: newM3aEnv writes both for every adapter test) and the three
// verdicts of its decision table observed END TO END THROUGH THE ADAPTER (create-account, the test button, bulk add), each with the control that makes it mean something. The DNS is the fixture of the stack
// (service m3dns: tests/harness/fx serving m3adapter/m3dns.json; every domain below is made at run time under a name of this run, see m3_adapter_closeout_fromgate_helpers_test.go for why) and every step
// reads the fixture's own log of the questions module 3 asked, so that a verdict that did not come from DNS (a gate that is off, a cache that was never filled) cannot pass.
//
// WHAT THE MODULE DOES, READ FROM ITS CODE AND ITS DOCS (Docs/history/phase-4-from-gate.md section 1.6, api-contract.md "From-gate additions"), AND WHAT THIS TEST ASSERTS:
//
//	PASS    SPF record with the provider's token (an include) => `aligned`: the create is accepted and needs no acknowledgment, whatever the DMARC policy. The provider is the exact smtp_host string against
//	        module 3's table (provider.go): smtp.fastmail.com (the alias of Mailpit) and smtp.gmail.com (the alias of the controllable sink). Paired controls: the same domain read for the OTHER provider (a
//	        token of another provider is no token); the same domain and provider with a smtp_username of another organisational domain (Gmail's send-as-via-provider pattern, section 1.7: a token match alone
//	        is not trusted); an acknowledgment on an aligned verdict records nothing.
//	WARN    an SPF record without the provider's token, or an unmatched provider (smtp-sink) => `unaligned_warned`: create REFUSED with 422 `unprocessable`, details [{acknowledge_alignment_warning, required}],
//	        nothing created (M3-P4-OD-15); the same create with acknowledge_alignment_warning=true is created, `unaligned_warned`, with the acknowledgment recorded for that verdict. Routable only once
//	        acknowledged: proved with module 3's own routing predicate through its quote (capacity.eligible_accounts) on a VERIFIED account, before and after the acknowledgment. A record, even with p=reject,
//	        is a WARN and never a BLOCK (the ruling that downgraded the locked rule).
//	BLOCK   NO SPF record AND no DKIM at any of the provider's probe selectors AND DMARC p=reject, at a provider that has probe selectors => `blocked`. THE MODULE DOES NOT REFUSE THIS CREATE: section 8 rejected
//	        "refuse the write" (a refusal would discard a credential the user can fix with one PATCH and leave nothing to press Test on), so the account is created, stored `blocked` and never routable, and an
//	        acknowledgment changes nothing. The controls: DKIM present => WARN; p=quarantine => WARN; an SPF record that lacks the token => WARN; an unmatched provider => WARN (never BLOCK).
//	INDETERMINATE  a lookup that answers SERVFAIL or stays silent is ignorance, never absence, and is never cached: module 3 DECIDES NOTHING. CREATE PROCEEDS (documented, M3-P4-OD-14: a verification problem
//	        never blocks an otherwise valid create): the account exists with the verdict `unverified` and nothing recorded; the test button answers `indeterminate` with the closed code from_dns_unavailable
//	        (HTTP 200); a bulk row is created with from_alignment_state `indeterminate`. (Only PATCH refuses with from_dns_unavailable, and the adapter has no PATCH.) Paired control: the SAME account, the
//	        SERVFAIL lifted and a record added at run time, is judged on the next click, and an identical account whose record was NOT added is judged WARN: the record is what flips the verdict.
//	OFF     from_gate_mode=off writes `aligned` WITHOUT asking DNS and says so in the stored detail (disclosed as skipped): the WARN domain is accepted unacknowledged with no question in the fixture's log, and
//	        the same account is judged again, WARN, the moment the gate is back on: the gate is what refused it.
//
// THE USER-FACING CODES (C3) on every answer: each account, test result and bulk row goes through the closed-set checks written from the module's documentation (m3cAccountProblems, m3cTestProblems,
// m3cBulkProblems), each error through its typed fields, and every rendered sentence is scanned for internal markers and for the raw text of DNS (m3dRawDNSText).
//
// WHAT AN ACCOUNT AT A PROVIDER ALIAS CANNOT DO is to be verified (the worker validates the sink's certificate for a host that is not private-looking): the steps read its From verdict and only require that
// its credential check's words are in the closed sets. The default accounts of every other adapter test are aligned by the self-hosted exception instead (example.test; see createVerified) and verify.
func TestM3Adapter_FromGatePassWarnBlock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), m3aBudget)
	t.Cleanup(cancel)
	e := newM3aEnv(t, ctx)
	e.prepare(t, true, true)

	// the gate is really on: the two dials newM3aEnv wrote are what module 3 reads (a test that finds the gate off would prove nothing below)
	var mode, resolver string
	if err := e.db.QueryRow(ctx, "SELECT (SELECT value->>'v' FROM send_byo.config WHERE key = 'from_gate_mode'), (SELECT value->>'v' FROM send_byo.config WHERE key = 'from_dns_resolver')").Scan(&mode, &resolver); err != nil {
		t.Fatalf("read module 3's From-gate dials: %v", err)
	}
	if mode != "enforce" || resolver != m3dResolver {
		t.Fatalf("module 3's From-gate dials are from_gate_mode=%q from_dns_resolver=%q, want enforce and %s: newM3aEnv writes both for every adapter test", mode, resolver, m3dResolver)
	}
	// the module's own seeded default for the cache lifetimes: the steps below rely on a lookup being remembered, and on a failed one not being
	if pos, neg := e.dialInt(t, "from_dns_cache_ttl_sec"), e.dialInt(t, "from_dns_negative_cache_ttl_sec"); pos < 60 || neg < 60 {
		t.Fatalf("module 3's From-gate cache lifetimes are %d s and %d s: the steps of this test (fresh names, a record changed under a name not yet answered) are written for lifetimes of minutes (the seeded 3600 and 300)", pos, neg)
	}

	step := func(name string, fn func(t *testing.T, k *m3dKit)) {
		t.Run(name, func(t *testing.T) {
			start := time.Now()
			fn(t, &m3dKit{t: t, e: e, ctx: ctx})
			t.Logf("NOTE m3a fromgate step %s done in %s", name, time.Since(start).Round(time.Millisecond))
		})
	}

	// ---- PASS ----
	step("pass", func(t *testing.T, k *m3dKit) {
		tn := e.seedTenant(t, "fgpass", nil)

		// Fastmail's token, through an include the fixture serves
		dF := k.domain("passf")
		k.publish(dF, "v=spf1 include:"+m3dTokenFast+" ~all", "v=DMARC1; p=reject")
		seq := e.dnsSeq(t)
		af := k.created("PASS at the Fastmail alias (SPF includes its token, p=reject)", tn, k.acct(tn, "f", m3dHostFast, dF))
		k.wantVerdict("PASS at the Fastmail alias", af, m3.AlignmentAligned, "reject", false)
		qs := e.dnsQueriesSince(t, seq)
		m3dRequireAsked(t, "PASS (Fastmail)", qs, dF, "TXT", "answer")
		m3dRequireAsked(t, "PASS (Fastmail): the include", qs, m3dTokenFast, "TXT", "answer")
		m3dRequireAsked(t, "PASS (Fastmail): DMARC", qs, "_dmarc."+dF, "TXT", "answer")
		for _, sel := range []string{"fm1", "fm2", "fm3"} { // the provider's DKIM probe selectors: none exists, so all three are asked
			m3dRequireAsked(t, "PASS (Fastmail): DKIM probe "+sel, qs, sel+"._domainkey."+dF, "TXT", "nxdomain", "nodata")
		}
		tr, err := tn.C.TestAccount(ctx, af.ID, false)
		if err != nil {
			t.Fatalf("the test button on the PASS account failed (%s): %s", modclient.KindOf(err), e.raw.scrub(err.Error()))
		}
		k.cleanTest("the test button on the PASS account", tr)
		if tr.Alignment.State != m3.AlignmentAligned || tr.Alignment.Code != m3.AlignmentCodeNone || tr.Alignment.NotStored {
			t.Errorf("the test button on the PASS account answered the From verdict %+v, want aligned with no code and nothing unsaved (a re-check of the same records)", tr.Alignment)
		}

		// Google Workspace's token, created WITH an acknowledgment: an aligned verdict records none
		dG := k.domain("passg")
		k.publish(dG, "v=spf1 include:"+m3dTokenGoog+" -all", "v=DMARC1; p=none")
		seq = e.dnsSeq(t)
		ng := k.acct(tn, "g", m3dHostGoog, dG)
		ng.AcknowledgeAlignmentWarning = true
		ag := k.created("PASS at the Google alias (SPF includes its token, p=none), acknowledgment sent", tn, ng)
		k.wantVerdict("PASS at the Google alias", ag, m3.AlignmentAligned, "none", false)
		qs = e.dnsQueriesSince(t, seq)
		m3dRequireAsked(t, "PASS (Google)", qs, dG, "TXT", "answer")
		m3dRequireAsked(t, "PASS (Google): the include", qs, m3dTokenGoog, "TXT", "answer")
		m3dRequireAsked(t, "PASS (Google): DKIM probe", qs, "google._domainkey."+dG, "TXT", "nxdomain", "nodata")
		if ag.FromAlignment.AcknowledgedAt != nil || e.gateRow(t, ag.ID).Acked {
			t.Errorf("the aligned account carries an acknowledgment (adapter %v, module 3's table %v): an acknowledgment is recorded for a warning only", ag.FromAlignment.AcknowledgedAt, e.gateRow(t, ag.ID).Acked)
		}

		// the default accounts of every other adapter test: the self-hosted exception (example.test, the sinks are its MX or live below it)
		for _, h := range []struct{ label, host string }{{"self-mail", m3dHostMail}, {"self-ctl", e.sinkIP}} {
			a := e.createVerified(t, tn, h.label, h.host)
			m3aProblems(t, "the self-hosted account at "+h.host, m3cAccountProblems(a))
			if r := e.gateRow(t, a.ID); r.State != "aligned" || !r.routable() || r.GateMode != "" {
				t.Errorf("the self-hosted account at %s is %s in module 3's table, want aligned by a real check (the gate was asked, not skipped)", h.host, r)
			}
		}

		// control 1: the SAME domain and records read for the OTHER provider: Fastmail's token is no token of Google's
		k.refusedUnacked("the Fastmail domain read for the Google host", tn, k.acct(tn, "cross", m3dHostGoog, dF))
		// control 2: the same domain, the same provider, a smtp_username of another organisational domain: the send-as-via-provider pattern is not trusted on a token match alone (section 1.7)
		via := k.acct(tn, "via", m3dHostFast, dF)
		via.SMTPUsername = "mailbox-" + e.runID + "@" + k.domain("elsewhere")
		k.refusedUnacked("the token matched but the authenticating mailbox is of another domain (send-as via the provider)", tn, via)
		if got := e.userAccounts(t, tn.UserID); got != 4 {
			t.Errorf("module 3 holds %d account(s) of the PASS tenant, want 4 (Fastmail, Google and the two self-hosted: the two controls made nothing)", got)
		}
	})

	// ---- WARN ----
	step("warn", func(t *testing.T, k *m3dKit) {
		tn := e.seedTenant(t, "fgwarn", nil)
		dW := k.domain("warn")
		k.publish(dW, "v=spf1 include:"+m3dNotAToken+" ~all", "v=DMARC1; p=reject") // a record WITHOUT the token, and p=reject: the ruling's WARN-not-BLOCK row
		seq := e.dnsSeq(t)
		n := k.acct(tn, "w", m3dHostFast, dW)
		k.refusedUnacked("WARN without acknowledgment at the Fastmail alias", tn, n)
		qs := e.dnsQueriesSince(t, seq)
		m3dRequireAsked(t, "WARN", qs, dW, "TXT", "answer")
		m3dRequireAsked(t, "WARN: the include that is no provider's", qs, m3dNotAToken, "TXT", "answer")
		m3dRequireAsked(t, "WARN: DMARC", qs, "_dmarc."+dW, "TXT", "answer")
		m3dRequireAsked(t, "WARN: DKIM probe", qs, "fm1._domainkey."+dW, "TXT", "nxdomain", "nodata")
		if got := e.userAccounts(t, tn.UserID); got != 0 {
			t.Fatalf("module 3 holds %d account(s) after the refused create, want 0", got)
		}

		// the control: the same account, acknowledged
		n.AcknowledgeAlignmentWarning = true
		a := k.created("WARN acknowledged at the Fastmail alias", tn, n)
		k.wantVerdict("WARN acknowledged at the Fastmail alias", a, m3.AlignmentWarned, "reject", true)
		if a.FromAlignment.Message == nil || strings.TrimSpace(*a.FromAlignment.Message) == "" {
			t.Errorf("the WARN verdict carries no sentence: the module renders hedged copy for a warning (Docs/history/phase-4-from-gate.md section 5.3)")
		}
		if r := e.gateRow(t, a.ID); !r.routable() || r.SPFAligned {
			t.Errorf("module 3's table says %s for the acknowledged warning, want the gate's term of its routing predicate true (an acknowledgment for the verdict in force) and the SPF leg not aligned", r)
		}

		// an unmatched provider is a WARN whatever the records say: the account at module 3's Mailpit sink by its own name (verified: the worker skips certificate validation for that name)
		un := k.acct(tn, "unmatched", m3dHostMail, dW)
		k.refusedUnacked("WARN: an unmatched provider (smtp-sink is in no row of the table)", tn, un)
		un.AcknowledgeAlignmentWarning = true
		au := k.created("WARN acknowledged at an unmatched provider", tn, un)
		k.wantVerdict("WARN at an unmatched provider", au, m3.AlignmentWarned, "reject", true)
		if au.Verification.State != m3.VerificationVerified {
			t.Fatalf("the account at %s is %q (code %q): module 3's worker skips certificate validation for this host name and the sink accepts any credential, so the routing proof below needs it verified", m3dHostMail, au.Verification.State, au.Verification.Code)
		}
		// routable: module 3's own quote (the dispatcher's predicate) counts the verified, acknowledged account and not the one that cannot be verified
		f, err := tn.C.Feasibility(ctx, 1)
		if err != nil {
			t.Fatalf("Feasibility through the adapter failed (%s): %s", modclient.KindOf(err), e.raw.scrub(err.Error()))
		}
		if !f.Feasible || f.Capacity.EligibleAccounts != 1 {
			t.Errorf("the quote of a tenant with one verified acknowledged warning and one unverifiable one is feasible %v with %d eligible account(s) (reasons %+v), want feasible with exactly 1: an acknowledged warning is routable", f.Feasible, f.Capacity.EligibleAccounts, f.Reasons)
		}
		if got := e.userAccounts(t, tn.UserID); got != 2 {
			t.Errorf("module 3 holds %d account(s) of the WARN tenant, want 2 (the acknowledged ones; the two refusals made nothing)", got)
		}
	})

	// ---- BLOCK ----
	step("block", func(t *testing.T, k *m3dKit) {
		tn := e.seedTenant(t, "fgblock", nil)
		dB := k.domain("block")
		k.publish(dB, "", "v=DMARC1; p=reject") // NO SPF record, no DKIM at google._domainkey, p=reject
		seq := e.dnsSeq(t)
		a := k.created("BLOCK at the Google alias (no SPF record, no DKIM selector, p=reject)", tn, k.acct(tn, "b", m3dHostGoog, dB))
		k.wantVerdict("BLOCK at the Google alias", a, m3.AlignmentBlocked, "reject", false)
		if a.FromAlignment.Message == nil || strings.TrimSpace(*a.FromAlignment.Message) == "" {
			t.Errorf("the BLOCK verdict carries no sentence: the module renders copy that says what it could not verify (section 5.2)")
		}
		qs := e.dnsQueriesSince(t, seq)
		m3dRequireAsked(t, "BLOCK: the SPF record that is not there", qs, dB, "TXT", "nodata", "nxdomain")
		m3dRequireAsked(t, "BLOCK: DMARC", qs, "_dmarc."+dB, "TXT", "answer")
		m3dRequireAsked(t, "BLOCK: DKIM probe", qs, "google._domainkey."+dB, "TXT", "nxdomain", "nodata")
		if got := e.userAccounts(t, tn.UserID); got != 1 {
			t.Errorf("module 3 holds %d account(s) after the BLOCK create, want 1: the module does not refuse a BLOCK (section 8 rejected refusing the write), it stores the account `blocked`", got)
		}
		if r := e.gateRow(t, a.ID); r.routable() || r.State != "blocked" {
			t.Errorf("module 3's table says %s for the BLOCK account, want blocked and not routable", r)
		}
		// an acknowledgment changes nothing about a BLOCK
		nb := k.acct(tn, "b2", m3dHostGoog, dB)
		nb.AcknowledgeAlignmentWarning = true
		ab := k.created("BLOCK again, with an acknowledgment sent", tn, nb)
		k.wantVerdict("BLOCK with an acknowledgment sent", ab, m3.AlignmentBlocked, "reject", false)
		if r := e.gateRow(t, ab.ID); r.Acked || r.routable() {
			t.Errorf("module 3's table says %s for the BLOCK account created with an acknowledgment: a BLOCK carries none and never routes", r)
		}
		// the test button re-reads it: still blocked (a 200, a verdict and no refusal)
		tr, err := tn.C.TestAccount(ctx, a.ID, true)
		if err != nil {
			t.Fatalf("the test button on the BLOCK account failed (%s): %s", modclient.KindOf(err), e.raw.scrub(err.Error()))
		}
		k.cleanTest("the test button on the BLOCK account", tr)
		if tr.Alignment.State != m3.AlignmentBlocked {
			t.Errorf("the test button on the BLOCK account answered %+v, want blocked (an acknowledgment cannot lift it)", tr.Alignment)
		}
		// never routable: the quote counts no eligible account (reasons: the accounts cannot be verified at the alias, and one that could would be refused as from_gate_blocked)
		f, err := tn.C.Feasibility(ctx, 1)
		if err != nil {
			t.Fatalf("Feasibility through the adapter failed (%s): %s", modclient.KindOf(err), e.raw.scrub(err.Error()))
		}
		if f.Feasible || f.Capacity.EligibleAccounts != 0 {
			t.Errorf("the quote of a tenant whose accounts are all BLOCKED is feasible %v with %d eligible account(s), want infeasible with none", f.Feasible, f.Capacity.EligibleAccounts)
		}

		// the controls, each a WARN and not a BLOCK: one condition of the BLOCK row removed at a time, the same host
		dD := k.domain("blockdkim")
		k.publish(dD, "", "v=DMARC1; p=reject")
		e.dnsPut(t, "google._domainkey."+dD, "TXT", "v=DKIM1; k=rsa; p=fixture") // DKIM evidence at the probe selector
		k.refusedUnacked("no SPF record, p=reject, but a DKIM selector exists", tn, k.acct(tn, "dkim", m3dHostGoog, dD))
		dQ := k.domain("blockq")
		k.publish(dQ, "", "v=DMARC1; p=quarantine") // p=quarantine no longer blocks
		k.refusedUnacked("no SPF record, no DKIM, but p=quarantine", tn, k.acct(tn, "quar", m3dHostGoog, dQ))
		dS := k.domain("blockspf")
		k.publish(dS, "v=spf1 include:"+m3dNotAToken+" ~all", "v=DMARC1; p=reject") // an SPF record that lacks the token is the common WARN case, never a BLOCK
		k.refusedUnacked("p=reject and an SPF record that lacks the token", tn, k.acct(tn, "spf", m3dHostGoog, dS))
		k.refusedUnacked("the BLOCK domain at an unmatched provider (it never blocks)", tn, k.acct(tn, "unm", m3dHostMail, dB))
		if got := e.userAccounts(t, tn.UserID); got != 2 {
			t.Errorf("module 3 holds %d account(s) of the BLOCK tenant, want 2 (the two BLOCK accounts; the four controls were refused and made nothing)", got)
		}
	})

	// ---- the DNS does not answer ----
	step("dns_unavailable_and_the_record_flips_the_verdict", func(t *testing.T, k *m3dKit) {
		tn := e.seedTenant(t, "fgdns", nil)
		dA, dC := k.domain("flipa"), k.domain("flipc")
		for _, d := range []string{dA, dC} { // identical: an SPF record without the token
			k.publish(d, "v=spf1 include:"+m3dNotAToken+" ~all", "v=DMARC1; p=none")
			e.dnsMode(t, d, "servfail")
		}
		seq := e.dnsSeq(t)
		aA := k.created("create while the DNS answers SERVFAIL (A)", tn, k.acct(tn, "a", m3dHostFast, dA))
		aC := k.created("create while the DNS answers SERVFAIL (C)", tn, k.acct(tn, "c", m3dHostFast, dC))
		for _, a := range []m3.Account{aA, aC} {
			k.wantNoVerdict("the account created while the DNS answered SERVFAIL", a)
		}
		qs := e.dnsQueriesSince(t, seq)
		m3dRequireAsked(t, "SERVFAIL (A)", qs, dA, "TXT", "servfail")
		m3dRequireAsked(t, "SERVFAIL (C)", qs, dC, "TXT", "servfail")
		if got := e.userAccounts(t, tn.UserID); got != 2 {
			t.Errorf("module 3 holds %d account(s), want 2: a create whose From check could not run proceeds (M3-P4-OD-14), it is not refused", got)
		}
		// the test button: 200 with `indeterminate` and the closed code; nothing is stored
		for _, a := range []m3.Account{aA, aC} {
			tr, err := tn.C.TestAccount(ctx, a.ID, false)
			if err != nil {
				t.Fatalf("the test button while the DNS answers SERVFAIL failed (%s): a From check that cannot run is a 200 with state indeterminate: %s", modclient.KindOf(err), e.raw.scrub(err.Error()))
			}
			k.cleanTest("the test button while the DNS answers SERVFAIL", tr)
			if tr.Alignment.State != m3.AlignmentIndeterminate || tr.Alignment.Code != m3.AlignmentDNSUnavailable || tr.Alignment.NotStored {
				t.Errorf("the test button while the DNS answers SERVFAIL gave %+v, want indeterminate with the closed code from_dns_unavailable and nothing unsaved", tr.Alignment)
			}
			got, err := tn.C.GetAccount(ctx, a.ID)
			if err != nil {
				t.Fatalf("GetAccount: %s", e.raw.scrub(err.Error()))
			}
			k.wantNoVerdict("the account after an indeterminate test button (nothing was stored)", got)
		}

		// the flip: the token record is added to A only, the SERVFAIL is lifted from both: the names have no cached answer, so the next click is judged on what the fixture says now
		e.dnsPut(t, dA, "TXT", "v=spf1 include:"+m3dTokenFast+" include:"+m3dNotAToken+" ~all")
		e.dnsMode(t, dA, "normal")
		e.dnsMode(t, dC, "normal")
		seq = e.dnsSeq(t)
		trA, err := tn.C.TestAccount(ctx, aA.ID, false)
		if err != nil {
			t.Fatalf("the test button on A after the flip failed (%s): %s", modclient.KindOf(err), e.raw.scrub(err.Error()))
		}
		k.cleanTest("the test button on A after the token record was added", trA)
		if trA.Alignment.State != m3.AlignmentAligned {
			t.Errorf("the test button on A, whose record now holds Fastmail's token, answered %+v, want aligned", trA.Alignment)
		}
		trC, err := tn.C.TestAccount(ctx, aC.ID, false)
		if err != nil {
			t.Fatalf("the test button on C after the flip failed (%s): %s", modclient.KindOf(err), e.raw.scrub(err.Error()))
		}
		k.cleanTest("the test button on C (the control: no record added)", trC)
		if trC.Alignment.State != m3.AlignmentWarned {
			t.Errorf("the test button on C, whose record was NOT changed, answered %+v, want unaligned_warned (a record without the token): the added record is what flips A", trC.Alignment)
		}
		qs = e.dnsQueriesSince(t, seq)
		m3dRequireAsked(t, "after the flip (A)", qs, dA, "TXT", "answer")
		m3dRequireAsked(t, "after the flip (A): DMARC", qs, "_dmarc."+dA, "TXT", "answer") // fresh: the SERVFAIL stopped the check at the SPF record, so nothing below it was asked before
		m3dRequireAsked(t, "after the flip (A): DKIM probe", qs, "fm1._domainkey."+dA, "TXT", "nxdomain", "nodata")
		m3dRequireAsked(t, "after the flip (C)", qs, dC, "TXT", "answer")
		gA, errA := tn.C.GetAccount(ctx, aA.ID)
		gC, errC := tn.C.GetAccount(ctx, aC.ID)
		if errA != nil || errC != nil {
			t.Fatalf("GetAccount after the flip: %v / %v", errA, errC)
		}
		if gA.FromAlignment.State != m3.AlignmentAligned || gA.FromAlignment.CheckedAt == nil || !e.gateRow(t, aA.ID).routable() {
			t.Errorf("A after the flip is %q (checked %v), want aligned, stored by the test button and routable", gA.FromAlignment.State, gA.FromAlignment.CheckedAt)
		}
		// an unacknowledged warning found by the test button is PERSISTED (M3-P4-OD-17: it never refuses the click) and the account stays non-routable until the user acknowledges
		if gC.FromAlignment.State != m3.AlignmentWarned || gC.FromAlignment.AcknowledgedAt != nil || e.gateRow(t, aC.ID).routable() {
			t.Errorf("C after the flip is %q (acknowledged %v, module 3's table says %s), want unaligned_warned, not acknowledged and not routable", gC.FromAlignment.State, gC.FromAlignment.AcknowledgedAt, e.gateRow(t, aC.ID))
		}
		trC2, err := tn.C.TestAccount(ctx, aC.ID, true)
		if err != nil {
			t.Fatalf("the test button on C with the acknowledgment failed (%s): %s", modclient.KindOf(err), e.raw.scrub(err.Error()))
		}
		k.cleanTest("the test button on C with the acknowledgment", trC2)
		gC2, err := tn.C.GetAccount(ctx, aC.ID)
		if err != nil || trC2.Alignment.State != m3.AlignmentWarned || gC2.FromAlignment.AcknowledgedAt == nil || !e.gateRow(t, aC.ID).routable() {
			t.Errorf("C after the acknowledged click: the click answered %+v, the account is %q acknowledged %v (error %v), want unaligned_warned, acknowledged and routable", trC2.Alignment, gC2.FromAlignment.State, gC2.FromAlignment.AcknowledgedAt, err)
		}

		// the resolver that stays SILENT is ignorance too: module 3 waits for its own lookup timeout, then decides nothing
		dN := k.domain("silent")
		k.publish(dN, "v=spf1 include:"+m3dTokenFast+" ~all", "v=DMARC1; p=none")
		e.dnsMode(t, dN, "silent")
		budget := time.Duration(e.dialInt(t, "from_dns_lookup_timeout_ms")) * time.Millisecond
		seq = e.dnsSeq(t)
		start := time.Now()
		aN := k.created("create while the DNS is silent", tn, k.acct(tn, "n", m3dHostFast, dN))
		waited := time.Since(start)
		k.wantNoVerdict("the account created while the DNS was silent", aN)
		m3dRequireAsked(t, "silence", e.dnsQueriesSince(t, seq), dN, "TXT", "silent")
		if waited < budget*8/10 {
			t.Errorf("the create against a silent DNS came back after %s, want it to have waited for module 3's lookup timeout (%s): a silent resolver is waited for, not guessed", waited.Round(time.Millisecond), budget)
		}
		t.Logf("NOTE m3a fromgate silence: the create waited %s for a resolver that never answered (module 3's lookup timeout %s) and then decided nothing", waited.Round(time.Millisecond), budget)
	})

	// ---- routable only once acknowledged, proved with module 3's own routing predicate ----
	step("an_unacknowledged_warning_is_not_routable_until_acknowledged", func(t *testing.T, k *m3dKit) {
		tn := e.seedTenant(t, "fgroute", nil)
		dR := k.domain("route")
		k.publish(dR, "v=spf1 include:"+m3dNotAToken+" ~all", "v=DMARC1; p=none")
		e.dnsMode(t, dR, "servfail") // the create is accepted without a verdict, so the account exists before any verdict does
		a := k.created("create at module 3's Mailpit sink while the DNS answers SERVFAIL", tn, k.acct(tn, "r", m3dHostMail, dR))
		k.wantNoVerdict("the account created while the DNS answered SERVFAIL", a)
		if a.Verification.State != m3.VerificationVerified {
			t.Fatalf("the account at %s is %q (code %q): this step needs a VERIFIED account (module 3 skips certificate validation for that host name), so that the From verdict is the only thing between it and the quote", m3dHostMail, a.Verification.State, a.Verification.Code)
		}
		quote := func(what string) m3.Feasibility {
			f, err := tn.C.Feasibility(ctx, 1)
			if err != nil {
				t.Fatalf("Feasibility (%s) through the adapter failed (%s): %s", what, modclient.KindOf(err), e.raw.scrub(err.Error()))
			}
			return f
		}
		reasons := func(f m3.Feasibility) []m3.InfeasibleCode {
			var out []m3.InfeasibleCode
			for _, r := range f.Reasons {
				out = append(out, r.Code)
			}
			return out
		}
		if f := quote("no verdict"); f.Feasible || f.Capacity.EligibleAccounts != 0 || !slices.Equal(reasons(f), []m3.InfeasibleCode{m3.InfeasibleNoEligibleAccounts}) {
			t.Errorf("a verified account with no From verdict: feasible %v, eligible %d, reasons %v, want infeasible with no_eligible_accounts (an unverified From is never routable: the predicate is a positive list)", f.Feasible, f.Capacity.EligibleAccounts, reasons(f))
		}
		e.dnsMode(t, dR, "normal")
		tr, err := tn.C.TestAccount(ctx, a.ID, false)
		if err != nil {
			t.Fatalf("the test button failed (%s): %s", modclient.KindOf(err), e.raw.scrub(err.Error()))
		}
		k.cleanTest("the test button that finds a warning", tr)
		if tr.Alignment.State != m3.AlignmentWarned || tr.Verification.State != m3.VerificationVerified {
			t.Fatalf("the test button answered verification %q and From verdict %+v, want verified and unaligned_warned (an unmatched provider)", tr.Verification.State, tr.Alignment)
		}
		if f := quote("an unacknowledged warning"); f.Feasible || f.Capacity.EligibleAccounts != 0 || !slices.Equal(reasons(f), []m3.InfeasibleCode{m3.InfeasibleNoEligibleAccounts}) {
			t.Errorf("a verified account with an UNACKNOWLEDGED warning: feasible %v, eligible %d, reasons %v, want infeasible with no_eligible_accounts: it stays non-routable until the user acknowledges", f.Feasible, f.Capacity.EligibleAccounts, reasons(f))
		}
		if r := e.gateRow(t, a.ID); r.State != "unaligned_warned" || r.Acked {
			t.Errorf("module 3's table says %s after the click that found the warning, want unaligned_warned and no acknowledgment", r)
		}
		if _, err := tn.C.TestAccount(ctx, a.ID, true); err != nil {
			t.Fatalf("the test button with the acknowledgment failed (%s): %s", modclient.KindOf(err), e.raw.scrub(err.Error()))
		}
		if f := quote("the acknowledged warning"); !f.Feasible || f.Capacity.EligibleAccounts != 1 {
			t.Errorf("the SAME account after the acknowledgment: feasible %v, eligible %d (reasons %v), want feasible with 1 eligible account: the acknowledgment is the only thing that changed", f.Feasible, f.Capacity.EligibleAccounts, reasons(f))
		}
		if r := e.gateRow(t, a.ID); !r.routable() {
			t.Errorf("module 3's table says %s after the acknowledgment, want an acknowledgment for the verdict in force", r)
		}
	})

	// ---- the gate switched off is what let the WARN domain in ----
	step("the_gate_off_accepts_what_the_gate_on_refused", func(t *testing.T, k *m3dKit) {
		tn := e.seedTenant(t, "fgoff", nil)
		dO := k.domain("off")
		k.publish(dO, "v=spf1 include:"+m3dNotAToken+" ~all", "v=DMARC1; p=none") // a WARN domain at a provider whose token it lacks
		var off m3.Account
		e.withDial(t, "from_gate_mode", `"off"`, func() {
			seq := e.dnsSeq(t)
			off = k.created("create at the WARN domain with the gate OFF, no acknowledgment", tn, k.acct(tn, "o", m3dHostFast, dO))
			if off.FromAlignment.State != m3.AlignmentAligned || off.FromAlignment.AcknowledgedAt != nil {
				t.Errorf("with from_gate_mode=off the WARN domain's create answered %q (acknowledged %v), want aligned with no acknowledgment: the check is skipped and disclosed as skipped", off.FromAlignment.State, off.FromAlignment.AcknowledgedAt)
			}
			if r := e.gateRow(t, off.ID); r.State != "aligned" || r.GateMode != "off" {
				t.Errorf("module 3's table says %s, want aligned with the detail naming from_gate_mode=off (disclosed as skipped, M3-P4-OD-12)", r)
			}
			m3dRequireNotAsked(t, "with the gate off", e.dnsQueriesSince(t, seq), dO)
		})
		// the gate is back on (the dial is restored): the same domain is refused for a second account, and the first is judged again on the next click
		// (the sequence number is taken BEFORE the refusal: its lookups fill module 3's cache, so the click after it asks nothing new, and the log of both shows the gate asking)
		seq := e.dnsSeq(t)
		k.refusedUnacked("the SAME WARN domain with the gate back on (another username)", tn, k.acct(tn, "o2", m3dHostFast, dO))
		tr, err := tn.C.TestAccount(ctx, off.ID, false)
		if err != nil {
			t.Fatalf("the test button failed (%s): %s", modclient.KindOf(err), e.raw.scrub(err.Error()))
		}
		k.cleanTest("the test button once the gate is back on", tr)
		if tr.Alignment.State != m3.AlignmentWarned {
			t.Errorf("the test button on the account that was accepted with the gate off answered %+v once the gate is back on, want unaligned_warned: the gate decides again", tr.Alignment)
		}
		m3dRequireAsked(t, "the gate back on", e.dnsQueriesSince(t, seq), dO, "TXT", "answer")
		if r := e.gateRow(t, off.ID); r.State != "unaligned_warned" || r.GateMode != "" || r.routable() {
			t.Errorf("module 3's table says %s after the click, want unaligned_warned from a real check (no skipped-by-policy note) and not routable", r)
		}
	})

	// ---- bulk add: the gate applies per row ----
	step("bulk_add_applies_the_gate_per_row", func(t *testing.T, k *m3dKit) {
		tn := e.seedTenant(t, "fgbulk", nil)
		dP, dW, dB, dD := k.domain("bulkpass"), k.domain("bulkwarn"), k.domain("bulkblock"), k.domain("bulkdown")
		k.publish(dP, "v=spf1 include:"+m3dTokenFast+" ~all", "v=DMARC1; p=none")
		k.publish(dW, "v=spf1 include:"+m3dNotAToken+" ~all", "v=DMARC1; p=none")
		k.publish(dB, "", "v=DMARC1; p=reject")
		k.publish(dD, "v=spf1 include:"+m3dTokenFast+" ~all", "v=DMARC1; p=none")
		e.dnsMode(t, dD, "servfail")
		warnAcked := k.acct(tn, "w2", m3dHostFast, dW)
		warnAcked.AcknowledgeAlignmentWarning = true
		rows := []m3.NewAccount{
			k.acct(tn, "p", m3dHostFast, dP),  // 1 PASS
			k.acct(tn, "w1", m3dHostFast, dW), // 2 WARN, no acknowledgment: rejected
			warnAcked,                         // 3 WARN acknowledged: created
			k.acct(tn, "b", m3dHostGoog, dB),  // 4 BLOCK: created, blocked
			k.acct(tn, "d", m3dHostFast, dD),  // 5 the DNS does not answer: created, nothing decided
		}
		seq := e.dnsSeq(t)
		res, err := tn.C.BulkAddAccounts(ctx, rows)
		if err != nil {
			t.Fatalf("BulkAddAccounts through the adapter failed (%s, may have been applied %v): %s", modclient.KindOf(err), m3.MaybeApplied(err), e.raw.scrub(err.Error()))
		}
		m3aProblems(t, "the bulk add of the From-gate rows", m3cBulkProblems(res))
		if len(res.Rows) != 5 {
			t.Fatalf("the bulk add answered %d row(s), want 5", len(res.Rows))
		}
		want := []struct {
			created bool
			state   m3.AlignmentState
		}{{true, m3.AlignmentAligned}, {false, ""}, {true, m3.AlignmentWarned}, {true, m3.AlignmentBlocked}, {true, m3.AlignmentIndeterminate}}
		for i, r := range res.Rows {
			if r.Created != want[i].created || r.Alignment != want[i].state {
				t.Errorf("bulk row %d is created %v with From verdict %q, want created %v with %q (row: %+v)", i+1, r.Created, r.Alignment, want[i].created, want[i].state, r)
			}
			if r.AlignmentMessage != nil {
				k.cleanText(fmt.Sprintf("bulk row %d's from_alignment_message", i+1), *r.AlignmentMessage)
			}
		}
		if r := res.Rows[1]; r.Reason != m3.RejectAlignmentWarning || !slices.Equal(r.Fields, []m3.FieldIssue{{Field: "acknowledge_alignment_warning", Issue: "required"}}) {
			t.Errorf("the unacknowledged WARN row is %+v, want rejected as from_alignment_warning_unacknowledged naming {acknowledge_alignment_warning, required}", r)
		}
		for _, i := range []int{2, 3, 4} {
			if m := res.Rows[i].AlignmentMessage; m == nil || strings.TrimSpace(*m) == "" {
				t.Errorf("bulk row %d (created, From verdict %q) carries no sentence about its From verdict", i+1, res.Rows[i].Alignment)
			}
		}
		if got := e.userAccounts(t, tn.UserID); got != 4 {
			t.Errorf("module 3 holds %d account(s) after the bulk add, want 4 (rows 1, 3, 4 and 5; row 2 made nothing)", got)
		}
		for i, wantState := range map[int]string{0: "aligned", 2: "unaligned_warned", 3: "blocked", 4: "unverified"} {
			r := e.gateRow(t, res.Rows[i].AccountID)
			if r.State != wantState || (i == 2) != r.Acked || (i == 0 || i == 2) != r.routable() {
				t.Errorf("bulk row %d: module 3's table says %s, want %s, acknowledged %v, routable %v", i+1, r, wantState, i == 2, i == 0 || i == 2)
			}
		}
		qs := e.dnsQueriesSince(t, seq)
		for _, d := range []string{dP, dW, dB} {
			m3dRequireAsked(t, "bulk add", qs, d, "TXT", "answer", "nodata", "nxdomain")
		}
		m3dRequireAsked(t, "bulk add (the row whose DNS did not answer)", qs, dD, "TXT", "servfail")
	})
}

// m3dKit is what the steps of the From-gate test share: the subtest's t, the env and the test's context.
type m3dKit struct {
	t   *testing.T
	e   *m3aEnv
	ctx context.Context
}

// domain is a From domain of this run: one label under .test, so that its organisational domain is itself (module 3 asks DMARC at the name, then at the organisational domain, which is the same name).
func (k *m3dKit) domain(label string) string { return fmt.Sprintf("fg%s-%s.test", k.e.runID, label) }

// publish writes the records the gate reads for a From domain into the fixture: its SPF record (none when spf is empty) and its DMARC record (none when dmarc is empty).
func (k *m3dKit) publish(domain, spf, dmarc string) {
	k.t.Helper()
	if spf != "" {
		k.e.dnsPut(k.t, domain, "TXT", spf)
	}
	if dmarc != "" {
		k.e.dnsPut(k.t, "_dmarc."+domain, "TXT", dmarc)
	}
}

// acct is an account whose From address and SMTP username are one mailbox of the domain, at host.
func (k *m3dKit) acct(tn *m3aTenant, label, host, domain string) m3.NewAccount {
	addr := fmt.Sprintf("sender-%s@%s", label, domain)
	name := "Harness"
	return m3.NewAccount{SMTPHost: host, SMTPPort: m3aSMTPPort, SMTPUsername: addr, SMTPSecret: config.Secret(k.e.sinkPass), FromAddress: addr, FromName: &name}
}

// created adds the account through the adapter, fails the step when the module refused it, and runs every answer through the closed-set checks.
func (k *m3dKit) created(what string, tn *m3aTenant, n m3.NewAccount) m3.Account {
	k.t.Helper()
	a, err := tn.C.CreateAccount(k.ctx, n)
	if err != nil {
		k.t.Fatalf("%s: CreateAccount through the adapter failed (%s, may have been applied: %v): %s", what, modclient.KindOf(err), m3.MaybeApplied(err), k.e.raw.scrub(err.Error()))
	}
	k.e.checkAccount(k.t, tn, n.SMTPHost, what, a)
	m3aProblems(k.t, what, m3cAccountProblems(a))
	if a.FromAlignment.Message != nil {
		k.cleanText(what+": the From verdict's sentence", *a.FromAlignment.Message)
	}
	if a.Verification.SMTPResponse != nil {
		if l := m3cLeaks(*a.Verification.SMTPResponse); len(l) != 0 {
			k.t.Errorf("%s: the provider text carries internal markers %v", what, l)
		}
	}
	k.t.Logf("NOTE m3a fromgate %s: from %s at %s: verdict %s (dmarc %q, acknowledged %v), credential check %s %s", what, a.FromAddress, a.SMTPHost, a.FromAlignment.State, a.FromAlignment.DMARCPolicy, a.FromAlignment.AcknowledgedAt != nil, a.Verification.State, a.Verification.Code)
	return a
}

// wantVerdict asserts what the account says about its From address: the state, the DMARC policy the gate read, a recorded check, and whether an acknowledgment is recorded; and that module 3's own table agrees.
func (k *m3dKit) wantVerdict(what string, a m3.Account, state m3.AlignmentState, policy string, acked bool) {
	k.t.Helper()
	al := a.FromAlignment
	if al.State != state || al.DMARCPolicy != policy || al.CheckedAt == nil || (al.AcknowledgedAt != nil) != acked {
		k.t.Errorf("%s: the account's From verdict is %q (dmarc policy %q, checked %v, acknowledged %v), want %q (policy %q, checked, acknowledged %v)", what, al.State, al.DMARCPolicy, al.CheckedAt != nil, al.AcknowledgedAt != nil, state, policy, acked)
	}
	if r := k.e.gateRow(k.t, a.ID); r.State != string(state) || r.Policy != policy || r.Acked != acked || r.GateMode != "" {
		k.t.Errorf("%s: module 3's own table says %s, want %s with policy %q, acknowledged %v, from a check that ran (no skipped-by-policy note)", what, r, state, policy, acked)
	}
}

// wantNoVerdict asserts an account module 3 decided nothing about: `unverified`, no check recorded, no acknowledgment, never routable.
func (k *m3dKit) wantNoVerdict(what string, a m3.Account) {
	k.t.Helper()
	al := a.FromAlignment
	if al.State != m3.AlignmentUnverified || al.CheckedAt != nil || al.AcknowledgedAt != nil || al.DMARCPolicy != "" {
		k.t.Errorf("%s: the account's From verdict is %q (dmarc policy %q, checked %v, acknowledged %v), want unverified with nothing recorded: module 3 must not invent a verdict from a lookup that did not answer", what, al.State, al.DMARCPolicy, al.CheckedAt != nil, al.AcknowledgedAt != nil)
	}
	if r := k.e.gateRow(k.t, a.ID); r.State != "unverified" || r.CheckedAt || r.Acked || r.routable() {
		k.t.Errorf("%s: module 3's own table says %s, want unverified, no check recorded, no acknowledgment, not routable", what, r)
	}
}

// refusedUnacked asserts the module's refusal of an unacknowledged warning: 422 `unprocessable` naming {acknowledge_alignment_warning, required}, a Refused error that says the request was not applied,
// nothing created, and nothing but the module's own closed words in the error.
func (k *m3dKit) refusedUnacked(what string, tn *m3aTenant, n m3.NewAccount) {
	k.t.Helper()
	before := k.e.userAccounts(k.t, tn.UserID)
	made, err := tn.C.CreateAccount(k.ctx, n)
	if err == nil {
		k.t.Errorf("%s: the create succeeded (account %s, From verdict %q), want the module's refusal of an unacknowledged warning: 422 unprocessable", what, made.ID, made.FromAlignment.State)
		return
	}
	me := m3aWantAnswer(k.t, what, err, modclient.Refused, 422, "unprocessable")
	if me == nil {
		return
	}
	if !slices.Equal(me.Details, []modclient.Detail{{Field: "acknowledge_alignment_warning", Issue: "required"}}) || m3.MaybeApplied(err) {
		k.t.Errorf("%s: the refusal names details %v (may have been applied: %v), want exactly [{acknowledge_alignment_warning required}] and a refusal that says nothing was done", what, me.Details, m3.MaybeApplied(err))
	}
	if l := m3cLeaks(err.Error()); len(l) != 0 {
		k.t.Errorf("%s: the refusal's text carries internal markers %v", what, l)
	}
	k.cleanText(what+": the refusal's text", err.Error())
	if after := k.e.userAccounts(k.t, tn.UserID); after != before {
		k.t.Errorf("%s: the refused create left %d account(s) of the tenant, %d before: a refused create makes nothing", what, after, before)
	}
	k.t.Logf("NOTE m3a fromgate %s: refused HTTP %d %s %v (request %s), nothing created", what, me.Status, me.Code, me.Details, me.RequestID)
}

// cleanTest runs a test button answer through the closed sets and its sentence through the text scan.
func (k *m3dKit) cleanTest(what string, r m3.TestResult) {
	k.t.Helper()
	m3aProblems(k.t, what, m3cTestProblems(r))
	if r.Alignment.Message != nil {
		k.cleanText(what+": the From verdict's sentence", *r.Alignment.Message)
	}
	if r.Verification.SMTPResponse != nil {
		if l := m3cLeaks(*r.Verification.SMTPResponse); len(l) != 0 {
			k.t.Errorf("%s: the provider text carries internal markers %v", what, l)
		}
	}
}

// cleanText asserts that a rendered sentence carries no internal marker and no raw text of the DNS: copy is built from the typed verdict, never from the records or the resolver's error.
func (k *m3dKit) cleanText(what, text string) {
	k.t.Helper()
	if l := m3cLeaks(text); len(l) != 0 {
		k.t.Errorf("%s carries internal markers %v: %q", what, l, text)
	}
	for _, raw := range m3dRawDNSText {
		if strings.Contains(text, raw) {
			k.t.Errorf("%s carries the raw DNS text %q: %q", what, raw, text)
		}
	}
}
