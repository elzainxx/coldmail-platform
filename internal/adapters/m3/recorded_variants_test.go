package m3_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/adapters/m3"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi/sendapitest"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// The tests of this file build a variant of a RECORDED body for something the recording does not hold (the harness records what the module answered when it was driven, and some answers
// were not reached). Each starts from the recorded file, changes only what the property needs, and has the recording itself as its control.

// A create conflict is a duplicate only when the module says so: it names smtp_username in the details (the module's create handler); the account cap and a suspension answer the same
// code with no details, which the adapter can only call a conflict whose cause is not stated. The two recordings are the two sides (the recorded duplicate names the username, the recorded
// opaque conflict does not), and each is the control of an injection into the other: the duplicate with its details taken out is not a duplicate, the opaque conflict with the detail put
// in is one, and a detail that names another field makes neither.
func TestACreateConflictIsADuplicateOnlyWhenTheModuleNamesTheUsername(t *testing.T) {
	dupBody, opaqueBody := doc(t, "accounts.create.error.duplicate.json"), doc(t, "accounts.create.error.conflict.json")
	if !namesUsername(t, dupBody) || namesUsername(t, opaqueBody) {
		t.Fatalf("the recordings are not the two sides of this test: the duplicate names the username: %v, the opaque conflict does: %v (the README must say what each recording is)",
			namesUsername(t, dupBody), namesUsername(t, opaqueBody))
	}
	create := func(body []byte) error {
		stub := sendapitest.NewStub(t, sendapitest.Serve(409, body))
		_, err := clientAt(t, stub.URL(), sendapitest.Token(t)).CreateAccount(ctxT(t), newAcct(t, 1))
		return err
	}
	withDetails := func(name string, details []any) []byte {
		return edit(t, readFixture(t, name), func(m map[string]any) {
			if details == nil {
				delete(sub(t, m, "error"), "details")
				return
			}
			sub(t, m, "error")["details"] = details
		})
	}
	username := []any{map[string]any{"field": "smtp_username", "issue": "conflict"}}
	otherField := []any{map[string]any{"field": "daily_limit", "issue": "conflict"}}
	for name, tc := range map[string]struct {
		body []byte
		dup  bool
	}{
		"the recorded duplicate":                       {readFixture(t, "accounts.create.error.duplicate.json"), true},
		"the recorded opaque conflict":                 {readFixture(t, "accounts.create.error.conflict.json"), false},
		"the duplicate with its details taken out":     {withDetails("accounts.create.error.duplicate.json", nil), false},
		"the opaque conflict with the username put in": {withDetails("accounts.create.error.conflict.json", username), true},
		"the duplicate naming another field":           {withDetails("accounts.create.error.duplicate.json", otherField), false},
	} {
		err := create(tc.body)
		wantKind(t, err, modclient.Refused)
		if errors.Is(err, m3.ErrAccountDuplicate) != tc.dup || errors.Is(err, sendapi.ErrConflictUnexplained) == tc.dup || errors.Is(err, m3.ErrActionUnavailable) {
			t.Fatalf("%s: duplicate=%v, got %v", name, tc.dup, err)
		}
	}
}

// What the module answers for a campaign it knows nothing about (a stranger's, a typo) is an empty 200, and the adapter can only say that nothing is known under the id. The recordings hold
// no such answer (they are of campaigns that exist), so it is the recorded whole-campaign page with its items and its counts taken out; the recording itself is the control: it is a campaign
// with items, it is known, and what the walk read matches its own total.
func TestAnEmptyCampaignIsNothingKnownAndARecordedOneIsNot(t *testing.T) {
	read := func(body []byte) sendapi.CampaignReport {
		stub := sendapitest.NewStub(t, sendapitest.Serve(200, body))
		r, err := clientAt(t, stub.URL(), sendapitest.Token(t)).ReportCampaign(ctxT(t), campaign)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	known := read(readFixture(t, terminalCampaign(t)))
	if known.NothingKnown() || !known.ItemsMatchTotal() || len(known.Items) == 0 {
		t.Fatalf("the recorded campaign: %+v", known)
	}
	empty := read(emptyCampaignBody(t))
	if !empty.NothingKnown() || !empty.ItemsMatchTotal() || empty.Progress.IsComplete || len(empty.Items) != 0 || len(empty.ByAccount) != 0 || empty.Pages != 1 {
		t.Fatalf("an empty campaign: %+v", empty)
	}
	same(t, "campaign_id", empty.CampaignID, campaign)
}

// A page with no items names no next cursor: a cursor on an empty page would not advance (the listing would never end), so it is refused, naming the key, for the list and for an
// account's events alike. The control is the same empty page with a null cursor, which is accepted and is an empty page, not a missing one. (The recording of the list is the last page of
// three accounts, and the recorded events page is empty; the empty accounts page is built from the recorded last page.)
func TestAnEmptyPageThatNamesACursorIsRefused(t *testing.T) {
	emptyList := func(next any) []byte {
		return edit(t, sendapitest.Read(t, fixtures, "accounts.list.empty.json"), func(m map[string]any) {
			m["items"] = []any{}
			m["next_cursor"] = next
		})
	}
	emptyEvents := func(next any) []byte {
		return edit(t, sendapitest.Read(t, fixtures, "accounts.events.empty.json"), func(m map[string]any) { m["next_cursor"] = next })
	}
	calls := map[string]struct {
		body func(next any) []byte
		call func(c *m3.Client) (any, error)
	}{
		"list":   {emptyList, func(c *m3.Client) (any, error) { return c.ListAccounts(ctxT(t), "") }},
		"events": {emptyEvents, func(c *m3.Client) (any, error) { return c.AccountEvents(ctxT(t), acct1, "") }},
	}
	for name, tc := range calls {
		serve := func(next any) (any, error) {
			stub := sendapitest.NewStub(t, sendapitest.Serve(200, tc.body(next)))
			return tc.call(clientAt(t, stub.URL(), sendapitest.Token(t)))
		}
		v, err := serve(nil)
		if err != nil {
			t.Fatalf("%s: an empty page with no cursor: %v", name, err)
		}
		switch p := v.(type) {
		case m3.AccountPage:
			if p.Items == nil || len(p.Items) != 0 || p.NextCursor != "" {
				t.Fatalf("%s: %+v", name, p)
			}
		case m3.EventPage:
			if p.Items == nil || len(p.Items) != 0 || p.NextCursor != "" {
				t.Fatalf("%s: %+v", name, p)
			}
		default:
			t.Fatalf("%s: %T", name, v)
		}
		v, err = serve("abc")
		if ce := wantKind(t, err, modclient.Contract); ce.Key != "next_cursor" || !isZero(v) {
			t.Fatalf("%s: an empty page that names a cursor: %v", name, err)
		}
	}
}

// The rows a bulk batch can hold that the recording does not: an account that was created and whose credential check FAILED (a code, the provider's answer, a warned From verdict), a created
// row with words the adapter does not know, and the rejections of the cap and of an unacknowledged warning. They are appended to the recorded answer and held to the document by the same
// comparison as the recorded rows, so a decoder that dropped a field, or invented one, is caught. The recording itself is the control: it passes the comparison alone.
func TestBulkRowsTheRecordingLacksAreDecodedFromTheRecordingPlusRows(t *testing.T) {
	recorded := sendapitest.Read(t, fixtures, "accounts.bulk.ok.json")
	n := bulkRows(t)
	extra := []map[string]any{
		{"status": "created", "account_id": norm3, "verification": "failed", "code": "verify_auth_failed", "smtp_response": "535 5.7.8 Authentication credentials invalid", "from_alignment_state": "unaligned_warned",
			"from_alignment_message": "The From address is not confirmed to match the sending server."},
		{"status": "created", "account_id": "00000000-0000-4000-8000-000000000004", "verification": "unverified", "from_alignment_state": "aligned"},
		{"status": "created", "account_id": "00000000-0000-4000-8000-000000000005", "verification": "brand_new_word", "code": "verify_brand_new", "from_alignment_state": "brand_new_state"},
		{"status": "rejected", "code": "account_cap_reached"},
		{"status": "rejected", "code": "from_alignment_warning_unacknowledged", "details": []any{map[string]any{"field": "acknowledge_alignment_warning", "issue": "required"}}},
	}
	body := edit(t, recorded, func(m map[string]any) {
		rows := m["results"].([]any)
		for _, e := range extra {
			e["row"] = len(rows) + 1
			rows = append(rows, e)
		}
		m["results"] = rows
	})
	run := func(b []byte, rows int) (m3.BulkResult, error) {
		stub := sendapitest.NewStub(t, sendapitest.Serve(200, b))
		return clientAt(t, stub.URL(), sendapitest.Token(t)).BulkAddAccounts(ctxT(t), batch(t, rows))
	}
	// the control: the recording alone
	res, err := run(recorded, n)
	if err != nil {
		t.Fatal(err)
	}
	bulkMatches(t, res, doc(t, "accounts.bulk.ok.json"))
	// the recording plus the rows it lacks
	res, err = run(body, n+len(extra))
	if err != nil {
		t.Fatal(err)
	}
	var d map[string]any
	if err := json.Unmarshal(body, &d); err != nil {
		t.Fatal(err)
	}
	bulkMatches(t, res, d)
	failed, unknown := res.Rows[n], res.Rows[n+2]
	if !failed.Created || failed.Verification != m3.VerificationFailed || failed.VerifyCode != m3.VerifyAuthFailed || failed.Alignment != m3.AlignmentWarned || failed.SMTPResponse == nil || failed.AlignmentMessage == nil {
		t.Fatalf("a created row whose credential failed: %+v", failed)
	}
	if unknown.Verification != m3.VerificationUnrecognised || unknown.VerifyCode != m3.VerifyUnrecognised || unknown.Alignment != m3.AlignmentUnrecognised || strings.Contains(reflectDump(unknown), "brand_new") {
		t.Fatalf("a created row with words the adapter does not know: %+v", unknown)
	}
	if res.Rows[n+3].Reason != m3.RejectAccountCap || res.Rows[n+4].Reason != m3.RejectAlignmentWarning || len(res.Rows[n+4].Fields) != 1 {
		t.Fatalf("the rejected rows: %+v %+v", res.Rows[n+3], res.Rows[n+4])
	}
}

// A single report's `completed_at` carries fractional seconds in the real module (the recording's fraction is normalised to zero), and a campaign item's does not: the instant is read with or
// without the fraction. The recorded single report with a real fraction in place of the normalised one is read to the microsecond; the control is the recording itself.
func TestAFractionalInstantIsReadToTheMicrosecond(t *testing.T) {
	recorded := sendapitest.Read(t, fixtures, "report.job.sent.json")
	read := func(body []byte) sendapi.Item {
		stub := sendapitest.NewStub(t, sendapitest.Serve(200, body))
		it, err := clientAt(t, stub.URL(), sendapitest.Token(t)).ReportJob(ctxT(t), jobItem)
		if err != nil {
			t.Fatal(err)
		}
		return it
	}
	if it := read(recorded); it.CompletedAt == nil || it.CompletedAt.Nanosecond() != 0 {
		t.Fatalf("the recording: %+v", it.CompletedAt)
	}
	withFraction := edit(t, recorded, func(m map[string]any) { m["completed_at"] = "2026-01-01T00:00:00.123456Z" })
	if it := read(withFraction); it.CompletedAt == nil || it.CompletedAt.Nanosecond() != 123456000 {
		t.Fatalf("a fractional instant: %+v", it.CompletedAt)
	}
}
