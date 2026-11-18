package m3_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/adapters/m3"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi/sendapitest"
)

// The accounts the harness recorded are healthy ones. What a user app must also read is an account that is paused, quarantined, blocked by the From gate, warned and acknowledged, with a
// failed credential and no from name: these are the recorded account with the fields the module sets for each state changed (the keys the module's account object has, by the module's own
// words), and each is held to the document it was decoded from by the same comparison as a recording, and to the typed value the state means. The recording itself is the control: it passes
// the comparison alone, and it is none of these states.
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
	// the control: the recording alone, and it is a healthy verified account
	a, d := read(base)
	accountMatches(t, a, d)
	if a.Paused || a.Quarantined || a.Disabled || a.Verification.State != m3.VerificationVerified || a.FromAlignment.State != m3.AlignmentAligned || a.PauseReason != "" || a.QuarantineReason != "" || a.QuarantinedAt != nil {
		t.Fatalf("the recorded account is not a healthy one: %+v", a)
	}

	// paused by the daily limit, with no from name
	paused := edit(t, base, func(m map[string]any) {
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
	quarantined := edit(t, base, func(m map[string]any) {
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
	warned := edit(t, base, func(m map[string]any) {
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

// doc2 decodes a body the way doc decodes a fixture file.
func doc2(t testing.TB, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	return m
}
