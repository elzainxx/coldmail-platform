//go:build harness

package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/adapters/m3"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// TestM3Adapter_UserVocabularyCodesOnly (Phase 3 close-out C3 / D5): the routes that put a status or a code in the BODY of a 200, the account test button and bulk add, answer user vocabulary only: every
// state and code is in the module's closed sets, and no field carries a piece of internal text (the raw reasons a failed create used to put in a bulk row's code, the slugs of a failed save of the From check).
// The adapter decodes those fields strictly (a word outside a set is a Contract error, never a guess), so a call that returns is itself evidence; this test also checks the typed values against the sets
// written out in m3_adapter_closeout_helpers_test.go (the module's own documentation, not the adapter's lists) and, for bulk add, the RAW body the module answered. What is driven, against the real module 3 and the
// controllable sink:
//
//	the test button    a click against the sink: verified, no code, aligned; a click while the sink refuses AUTH: failed with verify_auth_failed and the provider's own reply carried as opaque text
//	a failed create    an account whose port nothing listens on: created, verification failed, a closed connect code (verify_connect_failed, or verify_timeout where the network drops instead of refusing)
//	bulk add           one good row, one duplicate and one invalid row: created / rejected with duplicate_account_for_user / rejected with validation_failed and the field named; then, with the account cap lowered to what the
//	                   user holds, a row rejected with account_cap_reached
//	the raw body       a bulk add of the duplicate and the invalid row read from the wire: every string of the rows is in a closed set or is one of the two documented free-text fields (smtp_response and
//	                   from_alignment_message), and the check is shown to catch a row whose code is the raw reason of a failed insert
//
// What the harness cannot make module 3 say: verification state `unavailable` (code verify_error or verification_unavailable) needs the module's verifier to fail inside the module, the From states
// `unaligned_warned`, `blocked` and `indeterminate` need the From-gate ON with DNS fixtures (run.sh stage 5b writes from_gate_mode=off), and the infrastructure rejections (`internal_error`, `conflict` of a
// unique violation) need a failing statement inside module 3; the adapter's unit tests drive those words from recorded and built bodies, and the closed sets are held to the module's own list there.
func TestM3Adapter_UserVocabularyCodesOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), m3aBudget)
	t.Cleanup(cancel)
	e := newM3aEnv(t, ctx)
	e.prepare(t, true, true)

	tn := e.seedTenant(t, "voc", nil)
	first := e.createVerified(t, tn, "1", e.sinkIP)
	m3aProblems(t, "the created account", m3cAccountProblems(first))

	// ---- the test button ----
	ok, err := tn.C.TestAccount(ctx, first.ID, false)
	if err != nil {
		t.Fatalf("TestAccount through the adapter failed (%s): %s", modclient.KindOf(err), e.raw.scrub(err.Error()))
	}
	m3aProblems(t, "the test button against the sink", m3cTestProblems(ok))
	if ok.Verification.State != m3.VerificationVerified || ok.Verification.Code != m3.VerifyNone || ok.Alignment.State != m3.AlignmentAligned || ok.Alignment.Code != m3.AlignmentCodeNone || ok.Alignment.NotStored {
		t.Errorf("the test button against the sink answered %+v, want verified with no code, aligned with no code, nothing unsaved", ok)
	}
	e.setMode(t, m3hMode{FailAuth: true})
	refused, err := tn.C.TestAccount(ctx, first.ID, false)
	e.setMode(t, m3hMode{})
	if err != nil {
		t.Fatalf("TestAccount (the sink refuses AUTH) failed (%s): a failed credential is a 200: %s", modclient.KindOf(err), e.raw.scrub(err.Error()))
	}
	m3aProblems(t, "the test button against a sink that refuses AUTH", m3cTestProblems(refused))
	if refused.Verification.State != m3.VerificationFailed || refused.Verification.Code != m3.VerifyAuthFailed || refused.Verification.SMTPResponse == nil || !strings.Contains(*refused.Verification.SMTPResponse, "535") {
		t.Errorf("the test button against a sink that refuses AUTH answered %+v, want failed, verify_auth_failed and the provider's own reply (535) carried as text", refused)
	}
	if again, err := tn.C.TestAccount(ctx, first.ID, false); err != nil || again.Verification.State != m3.VerificationVerified { // the control: the sink accepts again
		t.Fatalf("the test button after the sink relented answered %+v (error %v), want verified", again, err)
	}
	// the control of the check itself: a state or a code outside the sets, and an unsaved verdict code that is not the closed word, are each reported
	for name, bad := range map[string]m3.TestResult{
		"a state outside the set":   {Verification: m3.TestVerification{State: "brand_new"}, Alignment: m3.TestAlignment{State: m3.AlignmentAligned}},
		"a code outside the set":    {Verification: m3.TestVerification{State: m3.VerificationFailed, Code: "unique_violation:accounts_pkey"}, Alignment: m3.TestAlignment{State: m3.AlignmentAligned}},
		"an alignment outside":      {Verification: m3.TestVerification{State: m3.VerificationVerified}, Alignment: m3.TestAlignment{State: "brand_new"}},
		"an alignment code outside": {Verification: m3.TestVerification{State: m3.VerificationVerified}, Alignment: m3.TestAlignment{State: m3.AlignmentIndeterminate, Code: "from_gate_write_error"}},
	} {
		if len(m3cTestProblems(bad)) == 0 {
			t.Errorf("the control: m3cTestProblems accepted %s", name)
		}
	}

	// ---- an account the worker cannot reach: created, failed, a closed connect code ----
	closed := e.newAccount("closed", tn, e.sinkIP)
	closed.SMTPPort = 1 // nothing listens there
	cl, err := tn.C.CreateAccount(ctx, closed)
	if err != nil {
		t.Fatalf("CreateAccount for a port nothing listens on failed (%s, may have been applied %v): %s (a failed verification is not an error: the account exists)", modclient.KindOf(err), m3.MaybeApplied(err), e.raw.scrub(err.Error()))
	}
	m3aProblems(t, "the account whose port is closed", m3cAccountProblems(cl))
	if cl.Verification.State != m3.VerificationFailed || (cl.Verification.Code != m3.VerifyConnectFailed && cl.Verification.Code != m3.VerifyTimeout) {
		t.Errorf("the account whose port is closed has verification %q code %q, want failed with verify_connect_failed (or verify_timeout where the network drops the packet)", cl.Verification.State, cl.Verification.Code)
	}
	t.Logf("NOTE m3a codes: the test button against the sink: %s/%q/%s; with AUTH refused: %s/%s (%q); an account on a closed port: %s/%s", ok.Verification.State, ok.Verification.Code, ok.Alignment.State, refused.Verification.State, refused.Verification.Code, m3aText(refused.Verification.SMTPResponse), cl.Verification.State, cl.Verification.Code)

	// ---- bulk add: one good row, one duplicate, one invalid ----
	good, dup, invalid := e.newAccount("bulk-good", tn, e.sinkIP), e.newAccount("1", tn, e.sinkIP), e.newAccount("bulk-bad", tn, e.sinkIP)
	invalid.FromAddress = "not-an-address"
	res, err := tn.C.BulkAddAccounts(ctx, []m3.NewAccount{good, dup, invalid})
	if err != nil {
		t.Fatalf("BulkAddAccounts through the adapter failed (%s, may have been applied %v): %s", modclient.KindOf(err), m3.MaybeApplied(err), e.raw.scrub(err.Error()))
	}
	m3aProblems(t, "the bulk add", m3cBulkProblems(res))
	if len(res.Rows) != 3 || !res.Rows[0].Created || res.Rows[0].Verification != m3.VerificationVerified || res.Rows[0].VerifyCode != m3.VerifyNone || res.Rows[0].Alignment != m3.AlignmentAligned {
		t.Fatalf("the bulk add answered %+v, want a created, verified, aligned first row and three rows", res.Rows)
	}
	if r := res.Rows[1]; r.Created || r.Reason != m3.RejectDuplicate {
		t.Errorf("the duplicate row is %+v, want rejected as duplicate_account_for_user", r)
	}
	if r := res.Rows[2]; r.Created || r.Reason != m3.RejectValidation || !slices.Contains(r.Fields, m3.FieldIssue{Field: "from_address", Issue: "malformed"}) {
		t.Errorf("the invalid row is %+v, want rejected as validation_failed naming {from_address, malformed}", r)
	}
	if got := e.userAccounts(t, tn.UserID); got != 3 {
		t.Errorf("module 3 holds %d account(s) of the tenant after the bulk add, want 3 (the first, the closed port and the good row): a rejected row creates nothing", got)
	}

	// ---- a row rejected because the account cap is reached ----
	e.withDial(t, "account_count_cap_per_user", fmt.Sprint(e.userAccounts(t, tn.UserID)), func() {
		capped, err := tn.C.BulkAddAccounts(ctx, []m3.NewAccount{e.newAccount("bulk-over-cap", tn, e.sinkIP)})
		if err != nil {
			t.Errorf("the bulk add at the account cap failed (%s): %s", modclient.KindOf(err), e.raw.scrub(err.Error()))
			return
		}
		m3aProblems(t, "the bulk add at the account cap", m3cBulkProblems(capped))
		if len(capped.Rows) != 1 || capped.Rows[0].Created || capped.Rows[0].Reason != m3.RejectAccountCap {
			t.Errorf("the row over the cap is %+v, want rejected as account_cap_reached", capped.Rows)
		}
	})

	// ---- the raw body of a bulk add: nothing but closed words and the two documented free-text fields ----
	row := func(n m3.NewAccount) map[string]any {
		return map[string]any{"smtp_host": n.SMTPHost, "smtp_port": n.SMTPPort, "smtp_username": n.SMTPUsername, "smtp_secret": e.sinkPass, "from_address": n.FromAddress, "from_name": "Harness"}
	}
	resp, err := e.rawDo(m3aRawCall{label: "m3.closeout.bulk", method: http.MethodPost, path: "/v1/send-byo/accounts/bulk", body: map[string]any{"user_id": tn.UserID, "accounts": []any{row(dup), row(invalid)}}})
	if err != nil {
		t.Fatalf("the raw bulk add failed: %s", e.raw.scrub(err.Error()))
	}
	m3aProblems(t, "the raw body of the bulk add", m3cRawBulkProblems(resp.Body))
	if len(m3cRawBulkProblems([]byte(`{"request_id":"req_1","results":[{"row":1,"status":"rejected","code":"unique_violation:accounts_user_host_key"}]}`))) == 0 {
		t.Error("the control: m3cRawBulkProblems accepted a rejected row whose code is the raw reason of a failed insert")
	}
	if got := e.userAccounts(t, tn.UserID); got != 3 {
		t.Errorf("module 3 holds %d account(s) after the raw bulk add of two rejected rows, want 3", got)
	}
	t.Logf("NOTE m3a codes bulk: rows %v / %s (%v) / %s (%v) then the capped row %s; the raw body of a bulk add of two rejected rows held only closed words", []bool{res.Rows[0].Created, res.Rows[1].Created, res.Rows[2].Created}, res.Rows[1].Reason, res.Rows[1].Fields, res.Rows[2].Reason, res.Rows[2].Fields, m3.RejectAccountCap)

	// every account the adapter lists is in the closed sets too
	all, err := tn.C.AllAccounts(ctx)
	if err != nil {
		t.Fatalf("AllAccounts: %s", e.raw.scrub(err.Error()))
	}
	for i, a := range all {
		m3aProblems(t, fmt.Sprintf("account %d of the list", i+1), m3cAccountProblems(a))
	}
}

// ---- the closed-set checks (they return problems, so the same function can be shown to catch a word outside a set) ----

func m3cAccountProblems(a m3.Account) []string {
	var p []string
	if !m3cIn([]string{"unverified", "verified", "failed"}, string(a.Verification.State)) {
		p = append(p, fmt.Sprintf("the account's verification state %q is not in the module's set", a.Verification.State))
	}
	if a.Verification.Code != "" && !m3cIn(m3cVerifyCodes, string(a.Verification.Code)) {
		p = append(p, fmt.Sprintf("the account's verification code %q is not in the module's set", a.Verification.Code))
	}
	if !m3cIn([]string{"unverified", "aligned", "unaligned_warned", "blocked"}, string(a.FromAlignment.State)) {
		p = append(p, fmt.Sprintf("the account's From verdict %q is not in the module's set", a.FromAlignment.State))
	}
	return p
}

func m3cTestProblems(r m3.TestResult) []string {
	var p []string
	if !m3cIn(m3cVerifyStates, string(r.Verification.State)) {
		p = append(p, fmt.Sprintf("verification.state %q is not in the closed set", r.Verification.State))
	}
	if r.Verification.Code != "" && !m3cIn(m3cVerifyCodes, string(r.Verification.Code)) {
		p = append(p, fmt.Sprintf("verification.code %q is not in the closed set", r.Verification.Code))
	}
	if !m3cIn(m3cAlignStates, string(r.Alignment.State)) {
		p = append(p, fmt.Sprintf("from_alignment.state %q is not in the closed set", r.Alignment.State))
	}
	if r.Alignment.Code != "" && !m3cIn(m3cAlignCodes, string(r.Alignment.Code)) {
		p = append(p, fmt.Sprintf("from_alignment.code %q is not in the closed set", r.Alignment.Code))
	}
	return p
}

func m3cLeaks(s string) []string {
	var out []string
	for _, m := range m3cInternalMarkers {
		if strings.Contains(s, m) {
			out = append(out, m)
		}
	}
	return out
}

func m3cBulkProblems(res m3.BulkResult) []string {
	var p []string
	for _, r := range res.Rows {
		where := fmt.Sprintf("row %d", r.Row)
		if r.Created {
			if !m3cIn(m3cVerifyStates, string(r.Verification)) {
				p = append(p, fmt.Sprintf("%s: verification %q is not in the closed set", where, r.Verification))
			}
			if r.VerifyCode != "" && !m3cIn(m3cVerifyCodes, string(r.VerifyCode)) {
				p = append(p, fmt.Sprintf("%s: code %q is not in the closed set", where, r.VerifyCode))
			}
			if r.Alignment != "" && !m3cIn(m3cAlignStates, string(r.Alignment)) {
				p = append(p, fmt.Sprintf("%s: from_alignment_state %q is not in the closed set", where, r.Alignment))
			}
		} else if !m3cIn(m3cBulkRejectCodes, string(r.Reason)) {
			p = append(p, fmt.Sprintf("%s: the rejection code %q is not in the closed set", where, r.Reason))
		}
		// every field but the two documented free-text ones (the provider's reply and the module's rendered sentence), rendered
		rendered := fmt.Sprintf("%v|%v|%v|%v|%v|%v|%v", r.Created, r.Reason, r.Fields, r.VerifyCode, r.Verification, r.Alignment, r.AccountID)
		if l := m3cLeaks(rendered); len(l) != 0 {
			p = append(p, fmt.Sprintf("%s carries internal text %v: %s", where, l, rendered))
		}
	}
	return p
}

// m3cRawBulkProblems reads a bulk add's answer as the wire carried it: the rows may hold the documented keys only, the status, the code, the verification and the From state in the closed sets, the detail
// words plain, and anything else in the two free-text keys.
func m3cRawBulkProblems(body []byte) []string {
	var doc struct {
		Results []map[string]json.RawMessage `json:"results"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return []string{fmt.Sprintf("the body is not the bulk answer: %v", err)}
	}
	if len(doc.Results) == 0 {
		return []string{"the bulk answer has no row: the subject of the check is empty"}
	}
	var p []string
	str := func(raw json.RawMessage) string {
		var s string
		_ = json.Unmarshal(raw, &s)
		return s
	}
	detailWords := []string{"required", "malformed", "out_of_range", "not_found", "conflict", "forbidden_value"}
	for i, row := range doc.Results {
		where := fmt.Sprintf("results[%d]", i)
		for key, raw := range row {
			v := str(raw)
			switch key {
			case "row", "account_id":
			case "status":
				if !m3cIn(m3cBulkRowStatuses, v) {
					p = append(p, fmt.Sprintf("%s.status %q is not in the closed set", where, v))
				}
			case "verification":
				if !m3cIn(m3cVerifyStates, v) {
					p = append(p, fmt.Sprintf("%s.verification %q is not in the closed set", where, v))
				}
			case "from_alignment_state":
				if !m3cIn(m3cAlignStates, v) {
					p = append(p, fmt.Sprintf("%s.from_alignment_state %q is not in the closed set", where, v))
				}
			case "code":
				if !m3cIn(m3cBulkRejectCodes, v) && !m3cIn(m3cVerifyCodes, v) {
					p = append(p, fmt.Sprintf("%s.code %q is not in the closed sets", where, v))
				}
			case "details":
				var ds []struct{ Field, Issue string }
				if err := json.Unmarshal(raw, &ds); err != nil {
					p = append(p, fmt.Sprintf("%s.details is not a list of {field, issue}: %v", where, err))
				}
				for _, d := range ds {
					if !m3cIn(detailWords, d.Issue) || d.Field == "" || strings.ContainsAny(d.Field, " :|") {
						p = append(p, fmt.Sprintf("%s.details has {%q, %q}, want a request field and a word of the issue vocabulary", where, d.Field, d.Issue))
					}
				}
			default:
				if !m3cBulkFreeTextKeys[key] {
					p = append(p, fmt.Sprintf("%s has the key %q, which is none of the documented keys", where, key))
				}
			}
		}
		if l := m3cLeaks(string(mustJSONWithoutFreeText(row))); len(l) != 0 {
			p = append(p, fmt.Sprintf("%s carries internal text %v", where, l))
		}
	}
	return p
}

// mustJSONWithoutFreeText is the row without its two free-text keys, for the scan of internal markers.
func mustJSONWithoutFreeText(row map[string]json.RawMessage) []byte {
	cp := map[string]json.RawMessage{}
	for k, v := range row {
		if k != "smtp_response" && k != "from_alignment_message" {
			cp[k] = v
		}
	}
	b, _ := json.Marshal(cp)
	return b
}
