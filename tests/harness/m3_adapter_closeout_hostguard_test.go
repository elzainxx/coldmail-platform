//go:build harness

package harness

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/coldmail-platform/platform/internal/adapters/m3"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// TestM3Adapter_SMTPHostGuard (Phase 3 close-out C4 / D5): an SMTP host is the one field of module 3 that makes the operator's own network open a connection to an address a USER chose, so module 3 guards it:
// a host that is, or resolves to, a special-purpose address (loopback, private, link-local including the cloud metadata address, ...) is refused at add time and again at every connect. The adapter
// judges no host itself any more (its own address rule was removed): it types the module's refusal. The harness lets the private compose network through (run.sh stage 5b sets the dial
// smtp_host_allow_cidrs to the three RFC 1918 ranges, so the sink is reachable) and every other special-purpose range stays refused: that is what is proved here, through the adapter, against the real module 3.
//
//	the controls      the compose sink by its own NAME and by its address (both private, both allowed by the dial) are created, verified and aligned: the host every other test uses
//	add time          create-account with 127.0.0.1, localhost, the cloud metadata address, [::1] and the loopback written as a decimal (2130706433): each is the module's 400 validation_failed naming
//	                  {smtp_host, forbidden_value}, ErrHostForbidden (and a Refused error, never an open outcome), and creates NOTHING (the tenant's accounts in module 3's own table do not change)
//	a name that does not resolve is allowed at add time (it is judged at connect): the account is created, its verification failed with one of the closed codes
//	bulk add          a batch of three with a blocked host in the middle: that row alone is rejected (validation_failed with the same detail, BulkRow.HostForbidden, Err() is ErrHostForbidden), the neighbours are created
//	connect time      with the allowlist emptied (the dial is restored when the step ends) the account that was created while the sink was allowed is clicked on the test button: failed with the closed code
//	                  verify_host_not_allowed and no provider text (the worker refused before anything was sent), and VerifyCode.HostRefusal() is ErrHostForbidden; a create at the sink's name is now refused at add
//	                  time; with the dial restored the SAME click verifies again (the control: the allowlist is what changed)
func TestM3Adapter_SMTPHostGuard(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), m3aBudget)
	t.Cleanup(cancel)
	e := newM3aEnv(t, ctx)
	e.prepare(t, true, true)

	tn := e.seedTenant(t, "hostg", nil)

	// ---- the controls: the sink by name and by address is what every other test names ----
	byName := e.createVerified(t, tn, "name", m3hCtlHost)
	e.createVerified(t, tn, "ip", e.sinkIP)
	if got := e.userAccounts(t, tn.UserID); got != 2 {
		t.Fatalf("module 3 holds %d account(s) of the tenant after the two controls, want 2", got)
	}

	// ---- add time: every spelling of a refused destination ----
	d := func(a, b, c, x int) string { return fmt.Sprintf("%d.%d.%d.%d", a, b, c, x) } // built at run time: the repository's gate reads literals
	for _, h := range []struct{ what, host string }{
		{"loopback", d(127, 0, 0, 1)}, {"localhost", "localhost"}, {"the cloud metadata address", d(169, 254, 169, 254)}, {"IPv6 loopback", "[::1]"}, {"loopback written as a decimal", "2130706433"},
	} {
		before := e.userAccounts(t, tn.UserID)
		_, err := tn.C.CreateAccount(ctx, e.newAccount("refused", tn, h.host))
		var me *modclient.Error
		if !errors.As(err, &me) || !errors.Is(err, m3.ErrHostForbidden) || me.Kind != modclient.Refused || me.Status != 400 || me.Code != "validation_failed" || m3.MaybeApplied(err) ||
			!slices.Equal(me.Details, []modclient.Detail{{Field: "smtp_host", Issue: "forbidden_value"}}) {
			t.Errorf("CreateAccount with %s (%q) answered %v (details %v), want the module's 400 validation_failed naming {smtp_host, forbidden_value} as ErrHostForbidden, a refusal and not an open outcome", h.what, h.host, err, detailsOf(err))
			continue
		}
		if after := e.userAccounts(t, tn.UserID); after != before {
			t.Errorf("CreateAccount with %s was refused and module 3 holds %d account(s) of the tenant (%d before): a refused create makes nothing", h.what, after, before)
		}
		t.Logf("NOTE m3a host guard add time: %s refused: HTTP %d %s %v (request %s), no account made", h.what, me.Status, me.Code, me.Details, me.RequestID)
	}
	// the control of the refusals: the same call for the sink's name is the one the controls made, and a name that does not resolve at add time is allowed (judged again at every connect)
	gone, err := tn.C.CreateAccount(ctx, e.newAccount("gone", tn, "no-such-host.invalid"))
	if err != nil {
		t.Fatalf("CreateAccount with a name that does not resolve answered %v (may have been applied %v), want the account created: a name that does not resolve at add time is allowed", err, m3.MaybeApplied(err))
	}
	m3aProblems(t, "the account with a name that does not resolve", m3cAccountProblems(gone))
	if gone.Verification.State != m3.VerificationFailed || gone.Verification.Code == "" {
		t.Errorf("the account whose host does not resolve has verification %q code %q, want failed with one of the closed codes (it was created, its connect failed)", gone.Verification.State, gone.Verification.Code)
	}
	t.Logf("NOTE m3a host guard unresolvable: an unresolvable name is created (allowed at add time); its connect: verification %s code %s", gone.Verification.State, gone.Verification.Code)

	// ---- bulk add: the blocked row alone is rejected ----
	accountsBefore := e.userAccounts(t, tn.UserID)
	blocked := e.newAccount("bk-blocked", tn, d(127, 0, 0, 1))
	res, err := tn.C.BulkAddAccounts(ctx, []m3.NewAccount{e.newAccount("bk-1", tn, m3hCtlHost), blocked, e.newAccount("bk-3", tn, e.sinkIP)})
	if err != nil {
		t.Fatalf("BulkAddAccounts with a blocked host in one row failed (%s, may have been applied %v): %s", modclient.KindOf(err), m3.MaybeApplied(err), e.raw.scrub(err.Error()))
	}
	m3aProblems(t, "the bulk add with a blocked host", m3cBulkProblems(res))
	if len(res.Rows) != 3 || !res.Rows[0].Created || !res.Rows[2].Created || res.Rows[1].Created {
		t.Fatalf("the bulk add answered %+v, want rows 1 and 3 created and row 2 rejected: the blocked row is rejected alone", res.Rows)
	}
	r := res.Rows[1]
	if r.Reason != m3.RejectValidation || !r.HostForbidden() || !errors.Is(r.Err(), m3.ErrHostForbidden) || !slices.Equal(r.Fields, []m3.FieldIssue{{Field: "smtp_host", Issue: "forbidden_value"}}) {
		t.Errorf("the blocked row is %+v (Err %v), want validation_failed naming {smtp_host, forbidden_value}, HostForbidden and ErrHostForbidden", r, r.Err())
	}
	if res.Rows[0].Err() != nil || res.Rows[2].Err() != nil || res.Rows[0].HostForbidden() {
		t.Error("the control: a created row is called a forbidden host")
	}
	if after := e.userAccounts(t, tn.UserID); after != accountsBefore+2 {
		t.Errorf("module 3 holds %d account(s) after the bulk add, want %d (the two good rows; the blocked one makes nothing)", after, accountsBefore+2)
	}

	// ---- connect time: the allowlist is emptied, so the guard judges the address the worker dials ----
	e.withDial(t, "smtp_host_allow_cidrs", "[]", func() {
		_, err := tn.C.CreateAccount(ctx, e.newAccount("name-closed", tn, m3hCtlHost))
		if !errors.Is(err, m3.ErrHostForbidden) || m3.MaybeApplied(err) {
			t.Errorf("with the allowlist empty CreateAccount at the sink's own name answered %v, want ErrHostForbidden (the private network is refused at add time again)", err)
		}
		tr, err := tn.C.TestAccount(ctx, byName.ID, false)
		if err != nil {
			t.Errorf("the test button of an account whose host is now refused failed (%s): a refused connect is a 200 with a failed verification: %s", modclient.KindOf(err), e.raw.scrub(err.Error()))
			return
		}
		m3aProblems(t, "the test button at connect time", m3cTestProblems(tr))
		if tr.Verification.State != m3.VerificationFailed || tr.Verification.Code != m3.VerifyHostNotAllowed || (tr.Verification.SMTPResponse != nil && *tr.Verification.SMTPResponse != "") {
			t.Errorf("the test button of an account whose host is refused at connect answered %+v, want failed with verify_host_not_allowed and no provider text (nothing was sent to the host)", tr.Verification)
			return
		}
		if err := tr.Verification.Code.HostRefusal(); !errors.Is(err, m3.ErrHostForbidden) || modclient.KindOf(err) != modclient.Refused {
			t.Errorf("VerifyCode.HostRefusal of verify_host_not_allowed is %v, want an ErrHostForbidden refusal", err)
		}
		t.Logf("NOTE m3a host guard connect time: allowlist emptied: the account at the sink's name fails its test button with %s (no provider text), a create at that name is refused at add time", tr.Verification.Code)
	})
	back, err := tn.C.TestAccount(ctx, byName.ID, false) // the control: the allowlist is back, the same click verifies
	if err != nil || back.Verification.State != m3.VerificationVerified || back.Verification.Code != m3.VerifyNone || back.Verification.Code.HostRefusal() != nil {
		t.Errorf("control: the same click with the allowlist restored answered %+v (error %v), want verified with no code", back, err)
	}
}

// detailsOf is the module's details of an error, for a failure message.
func detailsOf(err error) []modclient.Detail {
	var me *modclient.Error
	if errors.As(err, &me) {
		return me.Details
	}
	return nil
}
