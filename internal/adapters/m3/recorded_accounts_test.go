package m3_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/adapters/m3"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi/sendapitest"
)

// The account the harness recorded is one the real module answered for (see the README: it is a quarantined account whose credential and From verdict are good, the file named ok is the
// recorder's answer for that shape). What a user app must also read is a healthy account and an account that is paused, quarantined, blocked by the From gate, warned and acknowledged, with a
// failed credential and no from name: these are the recorded account with the fields the module sets for each state changed (the keys the module's account object has, by the module's own
// words), and each is held to the document it was decoded from by the same comparison as a recording, and to the typed value the state means. The recording is the control twice: it passes the
// comparison alone, and it is decoded to the state its own document says. The variants are built from a HEALTHY account made from the recording (the quarantine taken out), so that the state each
// one tests is the one its own edit puts there and not one the recording happens to hold.
func TestAccountsInStatesTheRecordingsLackAreDecodedFromTheRecordingPlusFields(t *testing.T) {
	base := readFixture(t, "accounts.get.ok.json")
	read := func(body []byte) (m3.Account, map[string]any) {
		stub := sendapitest.NewStub(t, sendapitest.Serve(200, body))
		a, err := clientAt(t, stub.URL(), sendapitest.Token(t)).GetAccount(ctxT(t), acct1)
		if err != nil {
			t.Fatal(err)
		}
		return a, doc2(t, body)
	}
	// the control: the recording alone is held to its own document, and the state it holds (read from the document) is the state that was decoded
	a, d := read(base)
	accountMatches(t, a, d)
	if quarantined := must(t, d, "status.quarantined").(bool); a.Quarantined != quarantined || (a.QuarantinedAt != nil) != quarantined || (a.QuarantineReason != "") != quarantined {
		t.Fatalf("the recorded account says quarantined = %v and decodes to %+v", quarantined, a)
	}

	// a healthy account: the recording with nothing wrong with it (the quarantine, and anything else that is not a healthy account's, taken out)
	healthy := edit(t, base, func(m map[string]any) {
		st := sub(t, m, "status")
		st["paused"], st["pause_reason"], st["quarantined"], st["disabled"] = false, nil, false, false
		delete(st, "quarantine_reason")
		delete(st, "quarantined_at")
		v := sub(t, m, "verification")
		v["state"], v["verified_at"] = "verified", "2026-01-01T00:00:00Z"
		delete(v, "code")
		al := sub(t, m, "from_alignment")
		al["state"] = "aligned"
		delete(al, "message")
		delete(al, "acknowledged_at")
	})
	a, d = read(healthy)
	accountMatches(t, a, d)
	if a.Paused || a.Quarantined || a.Disabled || a.Verification.State != m3.VerificationVerified || a.FromAlignment.State != m3.AlignmentAligned || a.PauseReason != "" || a.QuarantineReason != "" || a.QuarantinedAt != nil {
		t.Fatalf("the healthy account made from the recording is not a healthy one: %+v", a)
	}

	// paused by the daily limit, with no from name
	paused := edit(t, healthy, func(m map[string]any) {
		st := sub(t, m, "status")
		st["paused"], st["pause_reason"] = true, "daily_limit"
		m["from_name"] = nil
	})
	a, d = read(paused)
	accountMatches(t, a, d)
	if !a.Paused || a.PauseReason != "daily_limit" || a.FromName != nil || a.Quarantined {
		t.Fatalf("a paused account: %+v", a)
	}

	// quarantined for a rejected credential: the credential failed, the From gate blocks, and the module's quarantine keys are set (they are omitted otherwise)
	quarantined := edit(t, healthy, func(m map[string]any) {
		st := sub(t, m, "status")
		st["quarantined"], st["quarantine_reason"], st["quarantined_at"] = true, "auth_failure", "2026-01-01T00:00:00Z"
		v := sub(t, m, "verification")
		delete(v, "verified_at")
		v["state"], v["code"], v["smtp_response"] = "failed", "verify_auth_failed", "535 5.7.8 Authentication credentials invalid"
		al := sub(t, m, "from_alignment")
		al["state"], al["dmarc_policy"], al["message"] = "blocked", "reject", "The From address cannot be used with this server."
	})
	a, d = read(quarantined)
	accountMatches(t, a, d)
	if !a.Quarantined || a.QuarantineReason != "auth_failure" || a.QuarantinedAt == nil || !a.QuarantinedAt.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) ||
		a.Verification.State != m3.VerificationFailed || a.Verification.Code != m3.VerifyAuthFailed || a.Verification.VerifiedAt != nil ||
		a.FromAlignment.State != m3.AlignmentBlocked || a.FromAlignment.DMARCPolicy != "reject" || a.FromAlignment.Message == nil {
		t.Fatalf("a quarantined account: %+v", a)
	}

	// a From warning the user acknowledged, and an account the user disabled
	warned := edit(t, healthy, func(m map[string]any) {
		sub(t, m, "status")["disabled"] = true
		al := sub(t, m, "from_alignment")
		al["state"], al["acknowledged_at"] = "unaligned_warned", "2026-01-01T00:00:00Z"
	})
	a, d = read(warned)
	accountMatches(t, a, d)
	if !a.Disabled || a.FromAlignment.State != m3.AlignmentWarned || a.FromAlignment.AcknowledgedAt == nil {
		t.Fatalf("a warned and disabled account: %+v", a)
	}
}

// A create whose credential check failed is still a created account, with the reason the module gave. The recording of that answer is one reason (the real module's answer of that shape is a
// connect failure with no provider text, see the README), so every other code of the module's closed set is built from it: the recorded body with that code and, for a rejected credential, the
// provider's own text (read from the recorded failed test click). Each is held to the document it was decoded from and to the typed code; the recording is the control (decoded to the code it
// holds), and a word outside the set is refused by TestAWordOutsideTheModulesClosedSetsIsAContractError.
func TestACreatedAccountWhoseCredentialCheckFailedCarriesEveryCodeOfTheModule(t *testing.T) {
	base := readFixture(t, "accounts.create.created_failed.json")
	create := func(body []byte) m3.Account {
		stub := sendapitest.NewStub(t, sendapitest.Serve(201, body))
		a, err := clientAt(t, stub.URL(), sendapitest.Token(t)).CreateAccount(ctxT(t), newAcct(t, 1))
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	rec := doc(t, "accounts.create.created_failed.json")
	control := create(base)
	accountMatches(t, control, rec)
	if control.Verification.State != m3.VerificationFailed || string(control.Verification.Code) != asStr(t, rec, "verification.code") || control.Verification.VerifiedAt != nil {
		t.Fatalf("the recording: %+v", control.Verification)
	}
	provider := asStr(t, doc(t, "accounts.test.failed.json"), "verification.smtp_response") // what a rejected credential says
	codes := vocabSet(t, "verify_code")
	if len(codes) == 0 {
		t.Fatal("the closed set of verification codes is empty: the subject is empty")
	}
	for _, code := range codes {
		body := edit(t, base, func(m map[string]any) {
			v := sub(t, m, "verification")
			v["code"] = code
			if code == string(m3.VerifyAuthFailed) {
				v["smtp_response"] = provider
			}
		})
		a := create(body)
		accountMatches(t, a, doc2(t, body))
		if a.Verification.State != m3.VerificationFailed || string(a.Verification.Code) != code || a.Verification.VerifiedAt != nil || a.Verification.VerifyAt == nil {
			t.Fatalf("a create whose credential check failed with %s: %+v", code, a.Verification)
		}
		if code == string(m3.VerifyAuthFailed) && (a.Verification.SMTPResponse == nil || *a.Verification.SMTPResponse != provider) {
			t.Fatalf("a rejected credential: the provider's text was not carried: %+v", a.Verification)
		}
	}
}

// doc2 decodes a body the way doc decodes a fixture file.
func doc2(t testing.TB, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	return m
}
