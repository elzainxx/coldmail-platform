package sendapi_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi/sendapitest"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// Phase 3 close-out D5: the shared body's cancel (module 2 and, since C1, module 3), the causes of a run 409 (module 3, C2) and the optional campaign id of module 3's quote. Every property
// has its paired control: the same call that must fail with a changed input, or the one that must not change with the same input, is shown to behave the other way.

func cancelBody(campaign string, cancelled, inFlight, terminal int) string {
	return fmt.Sprintf(`{"campaign_id":%q,"cancelled":%d,"in_flight":%d,"already_terminal":%d}`, campaign, cancelled, inFlight, terminal)
}

// A cancel is the campaign's position after the call: the body names the campaign and THE CLIENT'S tenant (never another), the answer's three counts are returned as they are with no delta, a
// repeat is the same position (the call is idempotent, so a lost answer is retried), and an unknown or foreign campaign is {0, 0, 0}, which says only that nothing is known.
func TestCancelIsAPositionForTheClientsTenant(t *testing.T) {
	for name, p := range profiles() {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, p, userA, reply(200, cancelBody(campaign, 37, 3, 10)), modclient.Options{}, sendapi.Options{})
			pos, err := e.c.Cancel(ctxT(t), campaign)
			if err != nil {
				t.Fatal(err)
			}
			if pos != (sendapi.CancelPosition{CampaignID: campaign, Cancelled: 37, InFlight: 3, AlreadyTerminal: 10}) || pos.Total() != 50 || pos.Empty() {
				t.Fatalf("position %+v", pos)
			}
			reqs := e.stub.Requests()
			if len(reqs) != 1 || reqs[0].Method != http.MethodPost || reqs[0].Path != "/v1/cancel" {
				t.Fatalf("requests %+v", reqs)
			}
			var sent map[string]string
			if err := json.Unmarshal(reqs[0].Body, &sent); err != nil || len(sent) != 2 || sent["campaign_id"] != campaign || sent["user_id"] != userA {
				t.Fatalf("the body %s names the campaign and the tenant and nothing else (error %v)", reqs[0].Body, err)
			}
			// a second tenant's client puts ITS id in the body, never the first's
			eb := newEnv(t, p, userB, reply(200, cancelBody(campaign, 0, 0, 0)), modclient.Options{}, sendapi.Options{})
			zero, err := eb.c.Cancel(ctxT(t), campaign)
			if err != nil || !zero.Empty() || zero.Total() != 0 || zero.CampaignID != campaign {
				t.Fatalf("a foreign or unknown campaign: %+v %v", zero, err)
			}
			if p := sendapitest.CheckTenancy(eb.stub.Requests(), userB); len(p) != 0 {
				t.Fatalf("tenancy: %v", p)
			}
			if p := sendapitest.CheckTenancy(eb.stub.Requests(), userA); len(p) == 0 {
				t.Fatal("the control: the tenancy check does not catch a request for the other tenant")
			}
		})
	}
}

// A cancel is idempotent, so it is retried like a read: one 503 and then the position is the position, one logical call; a refusal is never retried; and no error carries a value.
func TestCancelIsRetriedOnlyWhereARetryCannotHarm(t *testing.T) {
	for name, p := range profiles() {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, p, userA, func(w http.ResponseWriter, _ *http.Request, n int) {
				if n == 1 {
					sendapitest.JSON(w, 503, sendapitest.Envelope("unavailable"))
					return
				}
				sendapitest.JSON(w, 200, cancelBody(campaign, 4, 0, 1))
			}, modclient.Options{Retries: 2}, sendapi.Options{})
			pos, err := e.c.Cancel(ctxT(t), campaign)
			if err != nil || pos.Cancelled != 4 || e.stub.Count() != 2 {
				t.Fatalf("a lost answer was not retried: %+v %v after %d request(s)", pos, err, e.stub.Count())
			}
			// the control: a 400 is the module's refusal, typed, with no value, and asked once
			r := newEnv(t, p, userA, reply(400, sendapitest.Envelope("validation_failed", [2]string{"campaign_id", "required"})), modclient.Options{Retries: 2}, sendapi.Options{})
			pos, err = r.c.Cancel(ctxT(t), campaign)
			ce := wantKind(t, err, modclient.Refused)
			if !isZero(pos) || ce.Code != "validation_failed" || r.stub.Count() != 1 || len(ce.Details) != 1 || ce.Details[0].Field != "campaign_id" {
				t.Fatalf("a refusal: %+v %v (%d request(s))", pos, err, r.stub.Count())
			}
		})
	}
}

// What the module answers is checked before it is believed: a count that is negative, a campaign other than the one cancelled and a body that is not the position are each a Contract
// error naming the key, with no value; the control is the same body unmutated.
func TestCancelRefusesAnAnswerThatIsNotThePosition(t *testing.T) {
	good := cancelBody(campaign, 5, 1, 2)
	for name, tc := range map[string]struct{ body, key string }{
		"another campaign":           {cancelBody(userB, 5, 1, 2), "campaign_id"},
		"a negative cancelled count": {cancelBody(campaign, -1, 1, 2), "cancelled"},
		"a negative in flight count": {cancelBody(campaign, 5, -1, 2), "in_flight"},
		"a negative terminal count":  {cancelBody(campaign, 5, 1, -2), "already_terminal"},
		"a count that is a string":   {`{"campaign_id":"` + campaign + `","cancelled":"5","in_flight":1,"already_terminal":2}`, "cancelled"},
		"a missing count":            {`{"campaign_id":"` + campaign + `","cancelled":5,"in_flight":1}`, "already_terminal"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, m3like(), userA, reply(200, tc.body), modclient.Options{}, sendapi.Options{})
			pos, err := e.c.Cancel(ctxT(t), campaign)
			if ce := wantKind(t, err, modclient.Contract); ce.Key != tc.key || !isZero(pos) {
				t.Fatalf("key %q, position %+v, want key %q and no value", ce.Key, pos, tc.key)
			}
		})
	}
	e := newEnv(t, m3like(), userA, reply(200, good), modclient.Options{}, sendapi.Options{})
	if pos, err := e.c.Cancel(ctxT(t), campaign); err != nil || pos.Cancelled != 5 {
		t.Fatalf("the control: the good body is refused: %+v %v", pos, err)
	}
}

// A campaign id that is not a lower-case uuid is refused before anything is sent (Invalid); a profile with no cancel route is NotSupported with nothing sent; the control is the same call
// on a profile that has the route.
func TestCancelRefusesBeforeAnyNetworkCall(t *testing.T) {
	e := newEnv(t, m3like(), userA, reply(200, cancelBody(campaign, 0, 0, 0)), modclient.Options{}, sendapi.Options{})
	for _, bad := range []string{"", "not-a-uuid", strings.ToUpper(campaign)} {
		if pos, err := e.c.Cancel(ctxT(t), bad); modclient.KindOf(err) != modclient.Invalid || !isZero(pos) {
			t.Fatalf("campaign id %q: %+v %v", bad, pos, err)
		}
	}
	if e.stub.Count() != 0 {
		t.Fatalf("%d request(s) for bad ids", e.stub.Count())
	}
	noRoute := m3like()
	noRoute.Cancel = false
	n := newEnv(t, noRoute, userA, reply(200, cancelBody(campaign, 0, 0, 0)), modclient.Options{}, sendapi.Options{})
	pos, err := n.c.Cancel(ctxT(t), campaign)
	wantKind(t, err, modclient.NotSupported)
	if !isZero(pos) || n.stub.Count() != 0 || !errors.Is(err, modclient.ErrNotSupported) {
		t.Fatalf("a profile without a cancel route: %+v %v after %d request(s)", pos, err, n.stub.Count())
	}
	if _, err := e.c.Cancel(ctxT(t), campaign); err != nil || e.stub.Count() != 1 {
		t.Fatalf("the control: a good id on a profile with the route: %v after %d request(s)", err, e.stub.Count())
	}
}

// A profile that has a cancel route lists the state a cancelled item lands in; one that lists the state without the route is fine (nothing can cancel, an item can still be reported so).
func TestAProfileWithCancelListsTheCancelledState(t *testing.T) {
	stub := sendapitest.NewStub(t, func(w http.ResponseWriter, _ *http.Request, _ int) { sendapitest.JSON(w, 500, "{}") })
	mc := sendapitest.ModClient(t, modclient.M3, stub.URL(), sendapitest.Token(t), modclient.Options{})
	bad := m3like()
	bad.JobStates = slices.DeleteFunc(slices.Clone(bad.JobStates), func(s string) bool { return s == "cancelled" })
	_, err := sendapi.New(mc, bad, userA, sendapi.Options{})
	if ce := wantKind(t, err, modclient.Invalid); !strings.Contains(ce.Message, "cancelled state") {
		t.Fatalf("the reason does not say why: %v", err)
	}
	bad.Cancel = false
	if _, err := sendapi.New(mc, bad, userA, sendapi.Options{}); err != nil {
		t.Fatalf("the control (no route, no state): %v", err)
	}
	ok := m3like()
	ok.Cancel = false
	if _, err := sendapi.New(mc, ok, userA, sendapi.Options{}); err != nil {
		t.Fatalf("the control (the state without the route): %v", err)
	}
}

// ---- the causes of a run 409 -------------------------------------------------------------------------------------------------

// conflictBody is the 409 of module 3's run: one entry per cause, in the module's order, the two caps with their numbers (the entries are {field, issue, code} and, for a cap, current and
// limit).
func conflictBody(causes ...string) string {
	var ds []string
	for _, c := range causes {
		d := fmt.Sprintf(`{"field":%q,"issue":"conflict","code":%q`, c, c)
		if c == "campaign_cap" || c == "queue_cap" {
			d += `,"current":50000,"limit":50000`
		}
		ds = append(ds, d+"}")
	}
	return `{"error":{"code":"conflict","message":"The request conflicts with the current state.","details":[` + strings.Join(ds, ",") + `],"request_id":"req_0123456789abcdef0123456789abcdef"}}`
}

var allCauses = []sendapi.Cause{sendapi.CauseSuspended, sendapi.CauseCampaignActive, sendapi.CauseCampaignCap, sendapi.CauseQueueCap}

var causeSentinels = map[sendapi.Cause]error{
	sendapi.CauseSuspended: sendapi.ErrRunSuspended, sendapi.CauseCampaignActive: sendapi.ErrRunCampaignActive, sendapi.CauseCampaignCap: sendapi.ErrRunCampaignCap, sendapi.CauseQueueCap: sendapi.ErrRunQueueCap,
}

// Module 3's 409 on run names its causes, and the typed error carries EXACTLY those: for each of the four, alone and in every combination the module can produce, errors.Is is true of the
// causes named and false of the others, the error is still the Refused 409 (Kind, status, code), the causes come back in the module's order, a refusal is asked once, and no result comes
// back. The control for each is that the other causes' sentinels are false for it.
func TestRunConflictNamesItsCauses(t *testing.T) {
	combos := [][]sendapi.Cause{{sendapi.CauseSuspended}, {sendapi.CauseCampaignActive}, {sendapi.CauseCampaignCap}, {sendapi.CauseQueueCap}, allCauses[:2], allCauses[1:], allCauses}
	for _, causes := range combos {
		name := fmt.Sprint(causes)
		t.Run(name, func(t *testing.T) {
			words := make([]string, len(causes))
			for i, c := range causes {
				words[i] = string(c)
			}
			e := newEnv(t, m3like(), userA, reply(409, conflictBody(words...)), modclient.Options{Retries: 2}, sendapi.Options{})
			r, err := e.c.Run(ctxT(t), jobItem)
			ce := wantKind(t, err, modclient.Refused)
			if !isZero(r) || ce.Status != 409 || ce.Code != "conflict" || !errors.Is(err, modclient.ErrRefused) {
				t.Fatalf("the refusal is %+v (result %+v)", ce, r)
			}
			if got := sendapi.CausesOf(err); !slices.Equal(got, causes) {
				t.Fatalf("causes %v, want %v", got, causes)
			}
			for _, c := range allCauses {
				if want := slices.Contains(causes, c); errors.Is(err, causeSentinels[c]) != want {
					t.Fatalf("errors.Is(%s) = %v, want %v", c, !want, want)
				}
			}
			if errors.Is(err, sendapi.ErrConflictUnexplained) || errors.Is(err, sendapi.ErrBackpressure) {
				t.Fatalf("a refusal that names its causes is also called unexplained or back-pressure: %v", err)
			}
			var cerr *sendapi.ConflictError
			if !errors.As(err, &cerr) || len(cerr.Causes) != len(causes) || !cerr.Has(causes[0]) {
				t.Fatalf("no ConflictError in the chain: %v", err)
			}
			if e.stub.Count() != 1 {
				t.Fatalf("a refusal was retried: %d requests", e.stub.Count())
			}
		})
	}
	// the paired control of the whole test: the same body with no details is NOT explained (a module older than the close-out), and says so
	e := newEnv(t, m3like(), userA, reply(409, sendapitest.Envelope("conflict")), modclient.Options{}, sendapi.Options{})
	_, err := e.c.Run(ctxT(t), jobItem)
	if !errors.Is(err, sendapi.ErrConflictUnexplained) || len(sendapi.CausesOf(err)) != 0 {
		t.Fatalf("a 409 with no details is explained: %v", err)
	}
	for _, c := range allCauses {
		if errors.Is(err, causeSentinels[c]) {
			t.Fatalf("a 409 with no details names %s", c)
		}
	}
}

// Module 2 never explains its 409 (owner ruling OD-5): the same body that module 3 reads as causes is back-pressure on module 2's profile and names none, so the platform never invents a
// cause for a module that does not say one.
func TestModule2NeverNamesACauseForItsConflict(t *testing.T) {
	e := newEnv(t, m2like(), userA, reply(409, conflictBody("suspended", "queue_cap")), modclient.Options{}, sendapi.Options{})
	_, err := e.c.Run(ctxT(t), jobItem)
	wantKind(t, err, modclient.Refused)
	if !errors.Is(err, sendapi.ErrBackpressure) || len(sendapi.CausesOf(err)) != 0 || errors.Is(err, sendapi.ErrRunSuspended) || errors.Is(err, sendapi.ErrRunQueueCap) || errors.Is(err, sendapi.ErrConflictUnexplained) {
		t.Fatalf("module 2's 409: %v", err)
	}
}

// A 409 that names something outside the closed vocabulary is a Contract error naming error.details[].field, never a cause the adapter picked: a word that is not a cause, one cause twice,
// an entry that is not a conflict, a cause word in the wrong place, and a body whose details are not an array of entries. The cause words are never in the message (the response text is
// never copied). The controls are the closed words themselves, in the test above, and a 409 whose code is not conflict (not explained at all).
func TestRunConflictOutsideTheClosedCausesIsAContractError(t *testing.T) {
	entry := func(field, issue string) string { return fmt.Sprintf(`{"field":%q,"issue":%q}`, field, issue) }
	body := func(entries ...string) string {
		return `{"error":{"code":"conflict","message":"x","details":[` + strings.Join(entries, ",") + `],"request_id":"req_1"}}`
	}
	for name, tc := range map[string]string{
		"a word that is not a cause":        body(entry("full_moon", "conflict")),
		"a known cause then an unknown one": body(entry("suspended", "conflict"), entry("full_moon", "conflict")),
		"one cause twice":                   body(entry("queue_cap", "conflict"), entry("queue_cap", "conflict")),
		"an entry that is not a conflict":   body(entry("suspended", "required")),
		"the detail of another field":       body(entry("user_id", "required")),
		"an upper-case cause":               body(entry("SUSPENDED", "conflict")),
		"a cause that is the issue":         body(entry("conflict", "suspended")),
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, m3like(), userA, reply(409, tc), modclient.Options{}, sendapi.Options{})
			r, err := e.c.Run(ctxT(t), jobItem)
			ce := wantKind(t, err, modclient.Contract)
			if ce.Key != "error.details[].field" || ce.Status != 409 || !isZero(r) || len(sendapi.CausesOf(err)) != 0 || errors.Is(err, sendapi.ErrConflictUnexplained) {
				t.Fatalf("key %q status %d: %v", ce.Key, ce.Status, err)
			}
			for _, w := range []string{"full_moon", "SUSPENDED"} {
				if strings.Contains(err.Error(), w) {
					t.Fatalf("the error carries a word of the response: %v", err)
				}
			}
		})
	}
	// control: the code is not `conflict`, so nothing is explained and nothing is called a cause
	e := newEnv(t, m3like(), userA, reply(409, `{"error":{"code":"unprocessable","message":"x","details":[{"field":"suspended","issue":"conflict"}],"request_id":"req_1"}}`), modclient.Options{}, sendapi.Options{})
	_, err := e.c.Run(ctxT(t), jobItem)
	if len(sendapi.CausesOf(err)) != 0 || errors.Is(err, sendapi.ErrRunSuspended) || errors.Is(err, sendapi.ErrConflictUnexplained) {
		t.Fatalf("a 409 that is not a conflict was explained: %v", err)
	}
}

// The closed set of causes is the module's vocabulary run_conflict_cause: the adapter's own list, in the module's order, a copy each time, every cause has a sentinel and a word outside
// the set has none.
func TestTheCausesAreTheClosedSet(t *testing.T) {
	got := sendapi.RunCauses()
	if !slices.Equal(got, allCauses) {
		t.Fatalf("causes %v", got)
	}
	got[0] = "tampered"
	if sendapi.RunCauses()[0] != sendapi.CauseSuspended {
		t.Fatal("RunCauses shares its slice between calls")
	}
	for _, c := range allCauses {
		if !c.Valid() || c.Sentinel() != causeSentinels[c] || c.Sentinel() == nil {
			t.Fatalf("the cause %s: valid %v sentinel %v", c, c.Valid(), c.Sentinel())
		}
	}
	for _, w := range []sendapi.Cause{"", "full_moon", "SUSPENDED", "queue_cap "} {
		if w.Valid() || w.Sentinel() != nil {
			t.Fatalf("%q is a cause", w)
		}
	}
	if !slices.Equal(m3like().Vocabularies()["run_conflict_cause"], []string{"suspended", "campaign_active", "campaign_cap", "queue_cap"}) {
		t.Fatalf("the profile's vocabulary: %v", m3like().Vocabularies()["run_conflict_cause"])
	}
	if _, ok := m2like().Vocabularies()["run_conflict_cause"]; ok {
		t.Fatal("module 2 has no run_conflict_cause vocabulary")
	}
}

// ---- the quote's optional campaign id ----------------------------------------------------------------------------------------

// Module 3's quote takes the campaign the recipients will join: QuoteForCampaign puts campaign_id in the body, Quote does not (the request is then byte for byte what it was before the
// close-out), a module whose quote takes no campaign id refuses the call before anything is sent, and an id that is not a lower-case uuid is Invalid.
func TestQuoteForCampaignSendsTheCampaignOnlyWhenGiven(t *testing.T) {
	quote := string(sendapitest.Read(t, fixtures, "quote.ok.json"))
	e := newEnv(t, m3like(), userA, reply(200, quote), modclient.Options{}, sendapi.Options{})
	if _, err := e.c.Quote(ctxT(t), 50); err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.QuoteForCampaign(ctxT(t), campaign, 50); err != nil {
		t.Fatal(err)
	}
	reqs := e.stub.Requests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests", len(reqs))
	}
	var plain, with map[string]any
	if err := json.Unmarshal(reqs[0].Body, &plain); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(reqs[1].Body, &with); err != nil {
		t.Fatal(err)
	}
	if _, has := plain["campaign_id"]; has || len(plain) != 2 || plain["user_id"] != userA || plain["recipient_count"] != float64(50) {
		t.Fatalf("a quote without a campaign sent %s", reqs[0].Body)
	}
	if with["campaign_id"] != campaign || len(with) != 3 || with["user_id"] != userA {
		t.Fatalf("a quote for a campaign sent %s", reqs[1].Body)
	}
	for _, bad := range []string{"", "nope", strings.ToUpper(campaign)} {
		if _, err := e.c.QuoteForCampaign(ctxT(t), bad, 50); modclient.KindOf(err) != modclient.Invalid {
			t.Fatalf("campaign id %q: %v", bad, err)
		}
	}
	if e.stub.Count() != 2 {
		t.Fatalf("bad ids reached the network: %d requests", e.stub.Count())
	}
	m2 := newEnv(t, m2like(), userA, reply(200, string(sendapitest.Read(t, "../m2/testdata/contract", "quote.ok.json"))), modclient.Options{}, sendapi.Options{})
	_, err := m2.c.QuoteForCampaign(ctxT(t), campaign, 10)
	wantKind(t, err, modclient.NotSupported)
	if m2.stub.Count() != 0 {
		t.Fatal("module 2 was asked for a quote for a campaign")
	}
	if _, err := m2.c.Quote(ctxT(t), 10); err != nil { // the control: module 2's plain quote is untouched
		t.Fatalf("module 2's quote: %v", err)
	}
}
