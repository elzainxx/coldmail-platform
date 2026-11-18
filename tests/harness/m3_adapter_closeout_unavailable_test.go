//go:build harness

package harness

import (
	"context"
	"testing"

	"github.com/coldmail-platform/platform/internal/adapters/m3"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// TestM3Adapter_TestButtonSaysUnavailableWhenTheCredentialCheckCannotRun (Phase 3 close-out D1): the one answer of the test button that no sink mode can make is the one where the credential check
// COULD NOT RUN (verification state `unavailable`, code `verify_error`; module 3 answers it when its verifier returns an error instead of a verdict). It used to be allowlisted as "cannot be elicited",
// and the contract drift gate then turned red on the runs where a slow DNS answer made the module give that answer by accident, and red again when the entry was removed on the runs where it did not:
// a contract that is exercised only by chance is not covered. This test makes the module give it on purpose, with a fault in module 3's OWN DATA (the way the module sees it, not a change of module
// 3): the stored credential of one account is damaged (its AEAD tag is flipped), so the module cannot decrypt it and cannot run the check.
//
//	the control      the same click on the same account before the damage: the credential check verifies, and the From verdict is `indeterminate` (the account's domain answers SERVFAIL in the fixture DNS, the
//	                 state the contract's fixture holds)
//	the fault        the damaged account: verification `unavailable` with the closed code `verify_error` and NO provider text (nothing was sent), the From verdict unchanged, still HTTP 200 (a check that could
//	                 not run is an answer, not an error), and nothing of the module's reason (the decrypt failure) in the body
//	the restore      the credential put back: the same click verifies again, so the damage is what changed
func TestM3Adapter_TestButtonSaysUnavailableWhenTheCredentialCheckCannotRun(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), m3aBudget)
	t.Cleanup(cancel)
	e := newM3aEnv(t, ctx)
	e.prepare(t, true, true)
	e.requireGateFixture(t)

	tn := e.seedTenant(t, "unavail", nil)
	k := &m3dKit{t: t, e: e, ctx: ctx}
	d := k.domain("unavail")
	k.publish(d, "v=spf1 include:"+m3dNotAToken+" ~all", "v=DMARC1; p=none")
	e.dnsMode(t, d, "servfail") // the create is accepted without a verdict (M3-P4-OD-14), so the account exists and verifies: the From check is the part that cannot run
	a := k.created("create at module 3's Mailpit sink while the DNS answers SERVFAIL", tn, k.acct(tn, "u", m3dHostMail, d))
	if a.Verification.State != m3.VerificationVerified {
		t.Fatalf("the account at %s is %q (code %q): this test needs a VERIFIED account, so that the damage below is the only reason a click could fail", m3dHostMail, a.Verification.State, a.Verification.Code)
	}

	click := func(what string) m3.TestResult {
		t.Helper()
		tr, err := tn.C.TestAccount(ctx, a.ID, false)
		if err != nil {
			t.Fatalf("%s: the test button failed (%s): a check that cannot run is a 200 with a word, never an error: %s", what, modclient.KindOf(err), e.raw.scrub(err.Error()))
		}
		k.cleanTest(what, tr)
		if tr.Alignment.State != m3.AlignmentIndeterminate || tr.Alignment.Code != m3.AlignmentDNSUnavailable {
			t.Errorf("%s: the From verdict is %+v, want indeterminate with from_dns_unavailable (the domain answers SERVFAIL)", what, tr.Alignment)
		}
		return tr
	}

	// the control
	if tr := click("the control, before the damage"); tr.Verification.State != m3.VerificationVerified || tr.Verification.Code != m3.VerifyNone {
		t.Fatalf("the control click gave the credential check %+v, want verified with no code", tr.Verification)
	}

	// the fault: flip the last byte of the stored ciphertext (the AEAD tag): the module cannot decrypt the credential. The original is kept and put back, whatever happens.
	var orig []byte
	if err := e.db.QueryRow(ctx, "SELECT credential_ciphertext FROM send_byo.accounts WHERE id = $1::uuid", a.ID).Scan(&orig); err != nil || len(orig) < 17 {
		t.Fatalf("read the account's stored credential (%d byte(s)): %v", len(orig), err)
	}
	restore := func() {
		if _, err := e.db.Exec(context.Background(), "UPDATE send_byo.accounts SET credential_ciphertext = $2 WHERE id = $1::uuid", a.ID, orig); err != nil {
			t.Errorf("could not put the account's stored credential back: %v", err)
		}
	}
	t.Cleanup(restore)
	if tag, err := e.db.Exec(ctx, "UPDATE send_byo.accounts SET credential_ciphertext = set_byte(credential_ciphertext, length(credential_ciphertext) - 1, get_byte(credential_ciphertext, length(credential_ciphertext) - 1) # 255) WHERE id = $1::uuid", a.ID); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("damage the account's stored credential: %v (rows %d)", err, tag.RowsAffected())
	}
	broken := click("the credential check that cannot run")
	if broken.Verification.State != m3.VerificationUnavailable || broken.Verification.Code != m3.VerifyError {
		t.Errorf("with the stored credential damaged the credential check is %+v, want state unavailable with the closed code verify_error", broken.Verification)
	}
	if broken.Verification.SMTPResponse != nil && *broken.Verification.SMTPResponse != "" {
		t.Errorf("the check that could not run carries provider text %q: nothing was sent, so there is none", *broken.Verification.SMTPResponse)
	}
	t.Logf("NOTE m3a unavailable: with the stored credential damaged the test button answers verification %s %s (From verdict %s %s), HTTP 200", broken.Verification.State, broken.Verification.Code, broken.Alignment.State, broken.Alignment.Code)

	// the restore: the same click verifies again
	restore()
	if tr := click("after the credential is put back"); tr.Verification.State != m3.VerificationVerified || tr.Verification.Code != m3.VerifyNone {
		t.Errorf("after the credential was put back the credential check is %+v, want verified with no code: the damage is what changed", tr.Verification)
	}
}
