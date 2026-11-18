package m3_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/adapters/m3"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi/sendapitest"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// Phase 3 close-out D5 for module 3: cancel (C1), the causes of a refused run and the quote that agrees with it (C2), the closed codes of the user routes (C3) and the destination guard's typed
// refusals (C4). Every property has its paired control.

// ---- cancel -------------------------------------------------------------------------------------------------------------------

func methodNames(v any) []string {
	var out []string
	rt := reflect.TypeOf(v)
	for i := 0; i < rt.NumMethod(); i++ {
		out = append(out, rt.Method(i).Name)
	}
	return out
}

// Module 3 has a cancel since the Phase 3 close-out (C1): the tenant's Client has Cancel (the shared body's, the same as module 2's) and the tenant-free Adapter has none (a cancel is always
// for a tenant's campaign). It is what the test that held the method absent held: the control runs the same listing on the Adapter, where the method must not be.
func TestCancelIsOnTheTenantsClientAndNotOnTheAdapter(t *testing.T) {
	for name, v := range map[string]any{"*Client": (*m3.Client)(nil), "*sendapi.Client": (*sendapi.Client)(nil)} {
		if !slices.Contains(methodNames(v), "Cancel") {
			t.Fatalf("%s has no Cancel: module 3 has POST /v1/cancel since the close-out (%v)", name, methodNames(v))
		}
	}
	if slices.Contains(methodNames((*m3.Adapter)(nil)), "Cancel") {
		t.Fatal("the tenant-free Adapter has a Cancel: a cancel is a tenant's")
	}
	m, ok := reflect.TypeOf((*m3.Client)(nil)).MethodByName("Cancel")
	if !ok || m.Type.NumIn() != 3 || m.Type.NumOut() != 2 || m.Type.Out(0) != reflect.TypeOf(sendapi.CancelPosition{}) {
		t.Fatalf("Cancel has the wrong signature: %v", m.Type)
	}
}

// A cancel of module 3's campaign sends the campaign and THE CLIENT'S tenant to POST /v1/cancel, returns the position as the module said it, and a second tenant's client sends its own id (the
// module then answers {0,0,0} for a campaign that is not its own, and so does the adapter's value: Empty, no error, no value of the first tenant).
func TestCancelOnModule3IsTenantScoped(t *testing.T) {
	campaign := asStr(t, doc(t, "cancel.ok.json"), "campaign_id")
	a := newStack(t, replyFixture(200, "cancel.ok.json", t), modclient.Options{})
	pos, err := a.client(t, userA).Cancel(ctxT(t), campaign)
	if err != nil || pos.CampaignID != campaign || pos.Cancelled != asInt(t, doc(t, "cancel.ok.json"), "cancelled") || pos.Empty() {
		t.Fatalf("cancel: %+v %v", pos, err)
	}
	reqs := a.stub.Requests()
	if len(reqs) != 1 || reqs[0].Method != http.MethodPost || reqs[0].Path != "/v1/cancel" {
		t.Fatalf("requests %+v", reqs)
	}
	var sent map[string]string
	if err := json.Unmarshal(reqs[0].Body, &sent); err != nil || len(sent) != 2 || sent["campaign_id"] != campaign || sent["user_id"] != userA {
		t.Fatalf("the body %s (%v)", reqs[0].Body, err)
	}
	b := newStack(t, reply(200, fmt.Sprintf(`{"campaign_id":%q,"cancelled":0,"in_flight":0,"already_terminal":0}`, campaign)), modclient.Options{})
	zero, err := b.client(t, userB).Cancel(ctxT(t), campaign)
	if err != nil || !zero.Empty() {
		t.Fatalf("a stranger's cancel: %+v %v", zero, err)
	}
	if got := tenancyProblems(b.stub.Requests(), userB); len(got) != 0 {
		t.Fatalf("tenancy: %v", got)
	}
	if got := tenancyProblems(b.stub.Requests(), userA); len(got) == 0 {
		t.Fatal("the control: the tenancy check does not catch a request of the other tenant")
	}
}

// ---- the quote that agrees with run -------------------------------------------------------------------------------------------

// FeasibilityForCampaign sends the campaign the recipients will join and Feasibility does not (the body is then what it was before the close-out); a campaign id that is not a lower-case uuid
// is refused before anything is sent. The reasons carry their causes and Causes/Predicts say what run would refuse for: the recorded infeasible quote predicts the campaign cap and the queue
// cap, and a feasible one predicts nothing.
func TestFeasibilityForACampaignAndTheCausesItPredicts(t *testing.T) {
	campaign := norm1
	n := int64(quoteCount(t, "quote.infeasible.json"))
	s := newStack(t, replyFixture(200, "quote.infeasible.json", t), modclient.Options{})
	c := s.client(t, userA)
	plain, err := c.Feasibility(ctxT(t), n)
	if err != nil {
		t.Fatal(err)
	}
	withCampaign, err := c.FeasibilityForCampaign(ctxT(t), campaign, n)
	if err != nil {
		t.Fatal(err)
	}
	reqs := s.stub.Requests()
	var p1, p2 map[string]any
	if err := json.Unmarshal(reqs[0].Body, &p1); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(reqs[1].Body, &p2); err != nil {
		t.Fatal(err)
	}
	if _, has := p1["campaign_id"]; has || len(p1) != 2 {
		t.Fatalf("a quote for a new campaign sent %s", reqs[0].Body)
	}
	if p2["campaign_id"] != campaign || len(p2) != 3 {
		t.Fatalf("a quote for a campaign sent %s", reqs[1].Body)
	}
	if !reflect.DeepEqual(plain, withCampaign) {
		t.Fatal("the same answer decodes to two values")
	}
	// the causes the recording names, read from it (the recorded quote is the queue depth cap's one reason: the real module's answer of that shape): the quote predicts exactly those, in the
	// module's order, and of the four causes of a run refusal it predicts each that the recording names and none that it does not
	rec := doc(t, "quote.infeasible.json")
	var want []sendapi.Cause
	for i := 0; i < length(t, rec, "reasons"); i++ {
		if c := optStr(rec, fmt.Sprintf("reasons[%d].cause", i)); c != nil {
			want = append(want, sendapi.Cause(*c))
		}
	}
	if len(want) == 0 {
		t.Fatal("the recorded infeasible quote names no cause: the subject is empty")
	}
	if got := plain.Causes(); !slices.Equal(got, want) {
		t.Fatalf("causes %v, the recording names %v", got, want)
	}
	for _, c := range sendapi.RunCauses() {
		if plain.Predicts(c) != slices.Contains(want, c) {
			t.Fatalf("Predicts(%s) = %v for a quote whose reasons name %v", c, plain.Predicts(c), want)
		}
	}
	// the campaign size cap is stated by the module now, and the two caps are two numbers (the recording's differ, so a decoder that read one for the other is caught here)
	if wantQ, wantC := asInt(t, rec, "caps.queue_depth_cap"), asInt(t, rec, "caps.campaign_size_cap"); wantC == 0 || wantQ == wantC || plain.Caps.CampaignSizeCap != wantC || plain.Caps.QueueDepthCap != wantQ {
		t.Fatalf("caps %+v: the recording says a queue depth cap of %d and a campaign size cap of %d (two different numbers, the second not zero)", plain.Caps, wantQ, wantC)
	}
	// the control: a quote that breaks both caps (built from the recording, which holds the queue cap's reason alone) predicts both causes, campaign size cap first, and still none of the other two
	both, nBoth := bothCapsQuote(t)
	fb, err := newStack(t, reply(200, string(both)), modclient.Options{}).client(t, userA).FeasibilityForCampaign(ctxT(t), campaign, nBoth)
	if err != nil || fb.Feasible || !slices.Equal(fb.Causes(), []sendapi.Cause{sendapi.CauseCampaignCap, sendapi.CauseQueueCap}) || !fb.Predicts(sendapi.CauseCampaignCap) || !fb.Predicts(sendapi.CauseQueueCap) ||
		fb.Predicts(sendapi.CauseSuspended) || fb.Predicts(sendapi.CauseCampaignActive) {
		t.Fatalf("a quote of both caps: %v %+v (causes %v)", err, fb, fb.Causes())
	}
	// the control: a feasible quote predicts no refusal
	ok := newStack(t, replyFixture(200, "quote.feasible.json", t), modclient.Options{})
	f, err := ok.client(t, userA).Feasibility(ctxT(t), int64(quoteCount(t, "quote.feasible.json")))
	if err != nil || !f.Feasible || len(f.Causes()) != 0 || f.Predicts(sendapi.CauseQueueCap) {
		t.Fatalf("a feasible quote: %+v %v", f, err)
	}
	for _, bad := range []string{"", "nope", strings.ToUpper(userB)} { // an upper-case spelling of a uuid that has letters
		if _, err := c.FeasibilityForCampaign(ctxT(t), bad, n); modclient.KindOf(err) != modclient.Invalid {
			t.Fatalf("campaign id %q: %v", bad, err)
		}
	}
	if s.stub.Count() != 2 {
		t.Fatalf("a bad campaign id reached the network: %d requests", s.stub.Count())
	}
}

// Each of the four causes as a reason of its own: its code, its cause, and the numbers of the two caps, read from a body the test builds from the recording (the recording has two of the four).
// A reason whose cause is not the one its code predicts, or is outside the closed set, is a Contract error naming the key; the same body with the right pairing is accepted (the control).
func TestEveryCauseIsAReasonOfItsOwn(t *testing.T) {
	base := sendapitest.Read(t, fixtures, "quote.infeasible.json")
	reasons := func(rs ...map[string]any) []byte {
		return edit(t, base, func(m map[string]any) {
			list := make([]any, len(rs))
			for i, r := range rs {
				list[i] = r
			}
			m["reasons"] = list
		})
	}
	run := func(body []byte) (m3.Feasibility, error) {
		s := newStack(t, reply(200, string(body)), modclient.Options{})
		return s.client(t, userA).Feasibility(ctxT(t), int64(quoteCount(t, "quote.infeasible.json")))
	}
	for _, tc := range []struct {
		reason map[string]any
		code   m3.InfeasibleCode
		cause  sendapi.Cause
	}{
		{map[string]any{"code": "suspended", "cause": "suspended"}, m3.InfeasibleSuspended, sendapi.CauseSuspended},
		{map[string]any{"code": "campaign_active", "cause": "campaign_active"}, m3.InfeasibleCampaignActive, sendapi.CauseCampaignActive},
		{map[string]any{"code": "campaign_cap", "cause": "campaign_cap", "limit": 5, "requested": 6, "current": 0, "cap_name": "campaign_size_cap"}, m3.InfeasibleCampaignCap, sendapi.CauseCampaignCap},
		{map[string]any{"code": "exceeds_queue_cap", "cause": "queue_cap", "limit": 5, "requested": 6, "current": 1, "cap_name": "queue_depth_cap"}, m3.InfeasibleExceedsQueueCap, sendapi.CauseQueueCap},
	} {
		f, err := run(reasons(tc.reason))
		if err != nil || len(f.Reasons) != 1 || f.Reasons[0].Code != tc.code || f.Reasons[0].Cause != tc.cause || f.Feasible || !slices.Equal(f.Causes(), []sendapi.Cause{tc.cause}) {
			t.Fatalf("%s: %+v %v", tc.code, f, err)
		}
		// the control of each: the same reason with another cause, and one outside the set
		for _, wrong := range []string{"suspended", "queue_cap", "campaign_active", "full_moon"} {
			if wrong == string(tc.cause) {
				continue
			}
			r := map[string]any{}
			for k, v := range tc.reason {
				r[k] = v
			}
			r["cause"] = wrong
			f, err := run(reasons(r))
			if ce := wantKind(t, err, modclient.Contract); ce.Key != "reasons[0].cause" || !isZero(f) {
				t.Fatalf("%s with the cause %s: %v", tc.code, wrong, err)
			}
		}
	}
	// a reason of a code that predicts no run refusal carries no cause and is read as before
	f, err := run(reasons(map[string]any{"code": "no_eligible_accounts"}))
	if err != nil || len(f.Reasons) != 1 || f.Reasons[0].Code != m3.InfeasibleNoEligibleAccounts || f.Reasons[0].Cause != "" || len(f.Causes()) != 0 {
		t.Fatalf("a reason with no cause: %+v %v", f, err)
	}
}

// The recorded infeasible quote holds one reason, so the breakages of a SECOND reason (the position the recording cannot reach) are made on a quote of both caps built from it: every damage to
// either reason (a code removed, a number of the wrong type or negative, a cause outside the closed set, missing, or the other reason's) is a Contract error naming that reason's own key, and the
// same body undamaged is the control (accepted, both reasons read).
func TestBothCapReasonsOfAQuoteAreHeldKeyByKey(t *testing.T) {
	both, n := bothCapsQuote(t)
	run := func(body []byte) (m3.Feasibility, error) {
		return newStack(t, reply(200, string(body)), modclient.Options{}).client(t, userA).Feasibility(ctxT(t), n)
	}
	if f, err := run(both); err != nil || len(f.Reasons) != 2 || f.Reasons[0].Cause != sendapi.CauseCampaignCap || f.Reasons[1].Cause != sendapi.CauseQueueCap {
		t.Fatalf("the control (the quote of both caps): %v %+v", err, f)
	}
	otherCause := []sendapi.Cause{sendapi.CauseQueueCap, sendapi.CauseCampaignCap}
	for i := 0; i < 2; i++ {
		at := func(k string) string { return fmt.Sprintf("reasons[%d].%s", i, k) }
		for _, c := range []struct {
			path  string
			op    sendapitest.Op
			value any
			key   string
		}{
			{at("code"), sendapitest.Delete, nil, at("code")},
			{at("limit"), sendapitest.Set, "x", at("limit")},
			{at("limit"), sendapitest.Set, -1, at("limit")},
			{at("requested"), sendapitest.Set, 1.5, at("requested")},
			{at("current"), sendapitest.Set, -1, at("limit")}, // a negative number is named by the reason's limit key, whichever of its numbers it is
			{at("current"), sendapitest.Set, "x", at("current")},
			{at("cause"), sendapitest.Set, "full_moon", at("cause")},
			{at("cause"), sendapitest.Delete, nil, at("cause")},
			{at("cause"), sendapitest.Set, string(otherCause[i]), at("cause")}, // the cause of the other reason
		} {
			body, err := sendapitest.Mutate(both, c.path, c.op, c.value)
			if err != nil {
				t.Fatal(err)
			}
			f, err := run(body)
			if ce := wantKind(t, err, modclient.Contract); ce.Key != c.key || !isZero(f) {
				t.Fatalf("%s %v %v: key %q, want %q: %v", c.path, c.op, c.value, ce.Key, c.key, err)
			}
		}
	}
}

// ---- the destination guard's typed refusals -----------------------------------------------------------------------------------

// The adapter judges no SMTP host (the module's destination guard does, at add time with a resolver and at every connect): every spelling is SENT, and the module's 400 naming {smtp_host,
// forbidden_value} is ErrHostForbidden (also a Refused error, nothing created, no open outcome). The controls: a 400 for another field, or another issue on the host, is NOT ErrHostForbidden.
func TestTheModulesHostRefusalIsTypedAndTheAdapterJudgesNoHost(t *testing.T) {
	d := func(a, b, c, e int) string { return fmt.Sprintf("%d.%d.%d.%d", a, b, c, e) } // built at run time: the repository's gate reads literals
	hosts := []string{d(127, 0, 0, 1), d(169, 254, 169, 254), d(10, 0, 0, 5), "localhost", "[::1]", "2130706433", "smtp-sink", "smtp.example.test"}
	for _, host := range hosts {
		s := newStack(t, reply(400, sendapitest.Envelope("validation_failed", [2]string{"smtp_host", "forbidden_value"})), modclient.Options{})
		n := newAcct(t, 1)
		n.SMTPHost = host
		_, err := s.client(t, userA).CreateAccount(ctxT(t), n)
		ce := wantKind(t, err, modclient.Refused)
		if !errors.Is(err, m3.ErrHostForbidden) || m3.MaybeApplied(err) || ce.Status != 400 || ce.Code != "validation_failed" || s.stub.Count() != 1 {
			t.Fatalf("%q: %v (%d requests)", host, err, s.stub.Count())
		}
		if strings.Contains(err.Error(), host) {
			t.Fatalf("%q: the error echoes the host", host)
		}
	}
	for name, body := range map[string]string{
		"another field":          sendapitest.Envelope("validation_failed", [2]string{"from_address", "malformed"}),
		"the host, malformed":    sendapitest.Envelope("validation_failed", [2]string{"smtp_host", "malformed"}),
		"the host, out of range": sendapitest.Envelope("validation_failed", [2]string{"smtp_host", "out_of_range"}),
		"no details":             sendapitest.Envelope("validation_failed"),
	} {
		s := newStack(t, reply(400, body), modclient.Options{})
		_, err := s.client(t, userA).CreateAccount(ctxT(t), newAcct(t, 1))
		if wantKind(t, err, modclient.Refused); errors.Is(err, m3.ErrHostForbidden) {
			t.Fatalf("%s is called a forbidden host", name)
		}
	}
	// the same issue in a 422 or a 409 is not the module's host refusal either
	for _, status := range []int{409, 422} {
		s := newStack(t, reply(status, sendapitest.Envelope("conflict", [2]string{"smtp_host", "forbidden_value"})), modclient.Options{})
		if _, err := s.client(t, userA).CreateAccount(ctxT(t), newAcct(t, 1)); errors.Is(err, m3.ErrHostForbidden) {
			t.Fatalf("a %d is called a forbidden host", status)
		}
	}
}

// A bulk row the destination guard refused is rejected ALONE: validation_failed with {smtp_host, forbidden_value}; BulkRow.HostForbidden and Err say so, the neighbours are created, and a
// rejected row for another reason (a malformed from address) is not a forbidden host. The code `verify_host_not_allowed` (the same refusal at connect time) is VerifyHostNotAllowed and its
// HostRefusal is an ErrHostForbidden Refused error; every other code, none included, has none.
func TestABulkRowAndAVerifyCodeCarryTheHostRefusal(t *testing.T) {
	recorded := sendapitest.Read(t, fixtures, "accounts.bulk.ok.json")
	body := edit(t, recorded, func(m map[string]any) {
		rows := m["results"].([]any)
		rows = append(rows,
			map[string]any{"row": len(rows) + 1, "status": "rejected", "code": "validation_failed", "details": []any{map[string]any{"field": "smtp_host", "issue": "forbidden_value"}}},
			map[string]any{"row": len(rows) + 2, "status": "created", "account_id": "00000000-0000-4000-8000-000000000009", "verification": "failed", "code": "verify_host_not_allowed", "from_alignment_state": "aligned"},
		)
		m["results"] = rows
	})
	n := bulkRows(t)
	s := newStack(t, reply(200, string(body)), modclient.Options{})
	res, err := s.client(t, userA).BulkAddAccounts(ctxT(t), batch(t, n+2))
	if err != nil {
		t.Fatal(err)
	}
	blocked, connect := res.Rows[n], res.Rows[n+1]
	if blocked.Created || blocked.Reason != m3.RejectValidation || !blocked.HostForbidden() || !errors.Is(blocked.Err(), m3.ErrHostForbidden) || modclient.KindOf(blocked.Err()) != modclient.Refused {
		t.Fatalf("the refused row: %+v", blocked)
	}
	if !connect.Created || connect.VerifyCode != m3.VerifyHostNotAllowed || connect.Verification != m3.VerificationFailed || connect.VerifyCode.HostRefusal() == nil || !errors.Is(connect.VerifyCode.HostRefusal(), m3.ErrHostForbidden) {
		t.Fatalf("the row refused at connect: %+v", connect)
	}
	// the controls: the recording's own rows are not host refusals (a duplicate, an invalid address on another field, two good ones), and no other code has a refusal
	for i := 0; i < n; i++ {
		if res.Rows[i].HostForbidden() || res.Rows[i].Err() != nil {
			t.Fatalf("row %d of the recording is a host refusal: %+v", i+1, res.Rows[i])
		}
	}
	for _, code := range []m3.VerifyCode{m3.VerifyNone, m3.VerifyAuthFailed, m3.VerifyConnectFailed, m3.VerifyError, m3.VerifyServiceUnavailable, m3.VerifyTimeout} {
		if code.HostRefusal() != nil {
			t.Fatalf("%q has a host refusal", code)
		}
	}
}

// ---- the closed codes of the user routes (C3) ---------------------------------------------------------------------------------

// Every state and code of the test button, of bulk add and of an account is in the module's closed set, and a word outside it is a Contract error naming its key: nothing is guessed and
// nothing of the word is carried. The control of each case is every word of the set, each accepted: a decoder that refused too much would be caught.
func TestAWordOutsideTheModulesClosedSetsIsAContractError(t *testing.T) {
	internal := "account_write_error: dial tcp 198.51.100.7:5432: connection refused"
	word := "brand_new_state_zzz"
	type probe struct {
		name    string
		fixture string
		path    string
		set     string // the closed set, by the adapter's name for it
		call    func(t testing.TB, url string) (any, error)
		key     string // the key the refusal names ("" is the path)
		inject  func(m map[string]any, v string)
	}
	test := func(t testing.TB, url string) (any, error) {
		return clientAt(t, url, sendapitest.Token(t)).TestAccount(ctxT(t), acct1, false)
	}
	get := func(t testing.TB, url string) (any, error) {
		return clientAt(t, url, sendapitest.Token(t)).GetAccount(ctxT(t), acct1)
	}
	list := func(t testing.TB, url string) (any, error) {
		return clientAt(t, url, sendapitest.Token(t)).ListAccounts(ctxT(t), "")
	}
	bulk := func(t testing.TB, url string) (any, error) {
		return clientAt(t, url, sendapitest.Token(t)).BulkAddAccounts(ctxT(t), batch(t, bulkRows(t)))
	}
	nested := func(parent, key string) func(m map[string]any, v string) {
		return func(m map[string]any, v string) {
			o, _ := m[parent].(map[string]any)
			if o == nil {
				o = map[string]any{}
				m[parent] = o
			}
			o[key] = v
		}
	}
	row := func(i int, key string) func(m map[string]any, v string) {
		return func(m map[string]any, v string) { m["results"].([]any)[i].(map[string]any)[key] = v }
	}
	probes := []probe{
		{"test verification state", "accounts.test.ok.json", "verification.state", "test_verification_state", test, "verification.state", nested("verification", "state")},
		{"test verification code", "accounts.test.failed.json", "verification.code", "verify_code", test, "verification.code", nested("verification", "code")},
		{"test alignment state", "accounts.test.ok.json", "from_alignment.state", "test_alignment_state", test, "from_alignment.state", nested("from_alignment", "state")},
		{"test alignment code", "accounts.test.unavailable.json", "from_alignment.code", "alignment_code", test, "from_alignment.code", nested("from_alignment", "code")},
		{"test alignment error", "accounts.test.failed.json", "from_alignment.error", "alignment_error", test, "from_alignment.error", nested("from_alignment", "error")},
		{"account verification state", "accounts.get.ok.json", "verification.state", "credential_verification_state", get, "verification.state", nested("verification", "state")},
		{"account verification code", "accounts.create.created_failed.json", "verification.code", "verify_code", get, "verification.code", nested("verification", "code")},
		{"account alignment state", "accounts.get.ok.json", "from_alignment.state", "from_verification_state", get, "from_alignment.state", nested("from_alignment", "state")},
		{"a listed account's verification state", "accounts.list.empty.json", "items[0].verification.state", "credential_verification_state", list, "items[0].verification.state", func(m map[string]any, v string) {
			m["items"].([]any)[0].(map[string]any)["verification"].(map[string]any)["state"] = v
		}},
		{"bulk verification", "accounts.bulk.ok.json", "results[0].verification", "test_verification_state", bulk, "results[0].verification", row(0, "verification")},
		{"bulk alignment state", "accounts.bulk.ok.json", "results[0].from_alignment_state", "test_alignment_state", bulk, "results[0].from_alignment_state", row(0, "from_alignment_state")},
		{"bulk verification code", "accounts.bulk.ok.json", "results[0].code", "verify_code", bulk, "results[0].code", row(0, "code")},
		{"bulk reject code", "accounts.bulk.ok.json", "results[1].code", "bulk_reject_code", bulk, "results[1].code", row(1, "code")},
	}
	for _, p := range probes {
		t.Run(p.name, func(t *testing.T) {
			base := sendapitest.Read(t, fixtures, p.fixture)
			status := 200
			serve := func(v string) (any, error) {
				body := edit(t, base, func(m map[string]any) { p.inject(m, v) })
				stub := sendapitest.NewStub(t, sendapitest.Serve(status, body))
				return p.call(t, stub.URL())
			}
			// the control: every word of the closed set is accepted (for the code sets: the words a created or rejected row can carry)
			for _, w := range sets(t, p.set) {
				if _, err := serve(w); err != nil {
					t.Fatalf("the word %q of the closed set %s is refused: %v", w, p.set, err)
				}
			}
			// a word outside it, and the module's internal text, are refused naming the key, with no value and none of the text
			for _, bad := range []string{word, internal, strings.ToUpper(sets(t, p.set)[0]), "unrecognised"} {
				v, err := serve(bad)
				ce := wantKind(t, err, modclient.Contract)
				if ce.Key != p.key || !isZero(v) || strings.Contains(fmt.Sprintf("%v %+v %#v", err, err, err), "zzz") || strings.Contains(fmt.Sprintf("%v", err), "dial tcp") {
					t.Fatalf("%q: key %q (want %q), value %+v: %v", bad, ce.Key, p.key, v, err)
				}
			}
		})
	}
}

// sets reads one closed set by the name the contracts give it, from the contracts themselves.
func sets(t testing.TB, name string) []string {
	t.Helper()
	return vocabSet(t, name)
}

// The test button's answers a user app shows: a click that verified, one that failed with each of the module's codes, one that could not run (`unavailable`, `verify_error`, with the From check's
// own code) and one that decided the From verdict but could not save it. Each is decoded to the typed value, with the provider's text and the module's sentence carried as opaque text.
func TestTheTestButtonAnswersAreDecodedFromTheClosedSets(t *testing.T) {
	read := func(body []byte) m3.TestResult {
		stub := sendapitest.NewStub(t, sendapitest.Serve(200, body))
		r, err := clientAt(t, stub.URL(), sendapitest.Token(t)).TestAccount(ctxT(t), acct1, false)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	failed := sendapitest.Read(t, fixtures, "accounts.test.failed.json")
	for _, code := range vocabSet(t, "verify_code") {
		r := read(edit(t, failed, func(m map[string]any) { m["verification"].(map[string]any)["code"] = code }))
		if string(r.Verification.Code) != code || r.Verification.State != m3.VerificationFailed || r.Verification.SMTPResponse == nil {
			t.Fatalf("code %s: %+v", code, r)
		}
	}
	u := read(sendapitest.Read(t, fixtures, "accounts.test.unavailable.json"))
	if u.Verification.State != m3.VerificationUnavailable || u.Verification.Code != m3.VerifyError || u.Alignment.State != m3.AlignmentIndeterminate || u.Alignment.Code != m3.AlignmentDNSUnavailable || u.Alignment.NotStored {
		t.Fatalf("a click that could not run: %+v", u)
	}
	saved := read(edit(t, failed, func(m map[string]any) { m["from_alignment"].(map[string]any)["error"] = "from_check_not_saved" }))
	if !saved.Alignment.NotStored {
		t.Fatalf("a verdict that was not saved: %+v", saved)
	}
	okRec := sendapitest.Read(t, fixtures, "accounts.test.ok.json")
	ok := read(okRec)
	if ok.Alignment.NotStored || ok.Alignment.Code != m3.AlignmentCodeNone {
		t.Fatalf("the control (the recording): %+v", ok)
	}
	testMatches(t, ok, doc(t, "accounts.test.ok.json"))
	// a click that verified. The file named ok is not one (the real module's answer of that shape is a credential check that failed, see the README), so it is built from it: the verification
	// is the module's success (state verified, no code, the provider's greeting taken from the recorded create that verified) and the From check is the recording's. The control is the recording
	// itself, read the same way and held to its own document above.
	greeting := asStr(t, doc(t, "accounts.create.created.json"), "verification.smtp_response")
	verifiedBody := edit(t, okRec, func(m map[string]any) {
		v := m["verification"].(map[string]any)
		delete(v, "code")
		v["state"], v["smtp_response"] = "verified", greeting
	})
	vr := read(verifiedBody)
	testMatches(t, vr, doc2(t, verifiedBody))
	if vr.Verification.State != m3.VerificationVerified || vr.Verification.Code != m3.VerifyNone || vr.Verification.SMTPResponse == nil || *vr.Verification.SMTPResponse != greeting ||
		vr.Alignment.State != ok.Alignment.State || vr.Alignment.NotStored {
		t.Fatalf("a click that verified: %+v", vr)
	}
	// the provider's reply and the module's sentence are opaque text: whatever they say they are carried (bounded) and never read as a code
	odd := read(edit(t, failed, func(m map[string]any) {
		m["verification"].(map[string]any)["smtp_response"] = "535 verify_host_not_allowed from_check_not_saved suspended"
		m["from_alignment"].(map[string]any)["message"] = "unavailable indeterminate"
	}))
	if odd.Verification.Code != m3.VerifyAuthFailed || odd.Alignment.NotStored || odd.Alignment.State != m3.AlignmentAligned || odd.Verification.SMTPResponse == nil || odd.Alignment.Message == nil {
		t.Fatalf("free text was read as a code: %+v", odd)
	}
}

// ---- describe -------------------------------------------------------------------------------------------------------------------

// Module 3's describe advertises the plain-text alternative (C5): the capability is read as the module says it, true in the recording and false when the module says false (it is not
// defaulted), and CheckVocabularies is not troubled by it.
func TestDescribeAdvertisesThePlainTextAlternative(t *testing.T) {
	good := sendapitest.Read(t, fixtures, "describe.ok.json")
	d, err := newStack(t, reply(200, string(good)), modclient.Options{}).a.Describe(ctxT(t))
	if err != nil || !d.Capabilities["plain_text_alternative"] {
		t.Fatalf("describe: %v %v", err, d)
	}
	off := edit(t, good, func(m map[string]any) { m["capabilities"].(map[string]any)["plain_text_alternative"] = false })
	d2, err := newStack(t, reply(200, string(off)), modclient.Options{}).a.Describe(ctxT(t))
	if v, ok := d2.Capabilities["plain_text_alternative"]; err != nil || !ok || v {
		t.Fatalf("a module that says false: %v %v %v", err, ok, v)
	}
	if rep, err := newStack(t, reply(200, string(good)), modclient.Options{}).a.CheckVocabularies(ctxT(t)); err != nil || rep.Breaking() {
		t.Fatalf("CheckVocabularies: %v %+v", err, rep)
	}
}
