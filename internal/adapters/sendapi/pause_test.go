package sendapi_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi/sendapitest"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// Phase 4, chunk D1: the shared body's Pause and Resume (POST /v1/pause and /v1/resume, module 2 send_managed_0031 and module 3 Phase 4 C1), the campaign report's additive `paused`, the per-module
// name of the pause capability, and the typed refusal of a run of a job item the platform marked cancelled (409 job_item_cancelled, Phase 4 D9).
//
// HAND-WRITTEN BODIES. Every body of the new shapes in this file is written by hand from the module's Docs/api-contract.md at its new head (module 2: "Pause and resume a campaign"; module 3:
// "POST /v1/pause and POST /v1/resume"; both: the campaign report's `paused`; both: "A job item the platform has cancelled"). They are NOT recordings: the lead re-records both modules after the pin
// and these tests are then joined by contract fixtures (the tests keep their own bodies, because they also write bodies no module sends). The names of the tests that depend on a hand-written body
// contain HandWritten so that the list of them is one grep.
//
// Every property below has its paired control: the same call with the one input changed must behave the other way.

// holdBody is the hand-written answer of POST /v1/pause or /v1/resume in the spelling of the profile's module: module 2's {campaign_id, paused, changed, state, queued, in_flight, terminal}
// and module 3's {campaign_id, state, paused}. The `state` word is the module's own (paused, or running) and is never read by the adapter.
func holdBody(p sendapi.Profile, id string, paused bool) string {
	state := "running"
	if paused {
		state = "paused"
	}
	if p.Module == modclient.M3 {
		return fmt.Sprintf(`{"campaign_id":%q,"state":%q,"paused":%t}`, id, state, paused)
	}
	return fmt.Sprintf(`{"campaign_id":%q,"paused":%t,"changed":true,"state":%q,"queued":7,"in_flight":1,"terminal":4}`, id, paused, state)
}

// verbs are the two calls and the answer each means when it took: a pause leaves the campaign held, a resume leaves it released.
var verbs = []struct {
	name, path string
	call       func(*sendapi.Client, *testing.T) (sendapi.PauseResult, error)
	heldAfter  bool
}{
	{"pause", "/v1/pause", func(c *sendapi.Client, t *testing.T) (sendapi.PauseResult, error) { return c.Pause(ctxT(t), campaign) }, true},
	{"resume", "/v1/resume", func(c *sendapi.Client, t *testing.T) (sendapi.PauseResult, error) { return c.Resume(ctxT(t), campaign) }, false},
}

// editJSON decodes a body into an object, lets fn change it (keys may be added, which Mutate does not do) and encodes it again.
func editJSON(t testing.TB, body []byte, fn func(m map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	fn(m)
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A pause and a resume are one POST each, to their own path, with the campaign and THE CLIENT'S tenant in the body and nothing else; the answer is the campaign's pause after the call, read from
// `paused` alone and echoing the campaign asked. The control is the same call answered with the opposite `paused`: the result follows the body, it is never assumed from the verb.
func TestPauseAndResumeHoldOneCampaignOfTheClientsTenantHandWritten(t *testing.T) {
	for name, p := range profiles() {
		for _, v := range verbs {
			t.Run(name+"/"+v.name, func(t *testing.T) {
				e := newEnv(t, p, userA, reply(200, holdBody(p, campaign, v.heldAfter)), modclient.Options{}, sendapi.Options{})
				got, err := v.call(e.c, t)
				if err != nil || got != (sendapi.PauseResult{CampaignID: campaign, Paused: v.heldAfter}) {
					t.Fatalf("%s: %+v %v", v.name, got, err)
				}
				reqs := e.stub.Requests()
				if len(reqs) != 1 || reqs[0].Method != http.MethodPost || reqs[0].Path != v.path {
					t.Fatalf("requests %+v, want one POST %s", reqs, v.path)
				}
				var sent map[string]string
				if err := json.Unmarshal(reqs[0].Body, &sent); err != nil || len(sent) != 2 || sent["campaign_id"] != campaign || sent["user_id"] != userA {
					t.Fatalf("the body %s names the campaign and the tenant and nothing else (error %v)", reqs[0].Body, err)
				}
				if p := sendapitest.CheckTenancy(e.stub.Requests(), userA); len(p) != 0 {
					t.Fatalf("tenancy: %v", p)
				}
				if p := sendapitest.CheckTenancy(e.stub.Requests(), userB); len(p) == 0 {
					t.Fatal("the control: the tenancy check does not catch a request for the other tenant")
				}
				// the control: the module says the opposite and so does the result
				o := newEnv(t, p, userA, reply(200, holdBody(p, campaign, !v.heldAfter)), modclient.Options{}, sendapi.Options{})
				if got, err := v.call(o.c, t); err != nil || got.Paused != !v.heldAfter {
					t.Fatalf("the control: the answer says paused=%t and the result is %+v %v", !v.heldAfter, got, err)
				}
				// a second tenant's client puts ITS id in the body
				b := newEnv(t, p, userB, reply(200, holdBody(p, campaign, false)), modclient.Options{}, sendapi.Options{})
				if _, err := v.call(b.c, t); err != nil {
					t.Fatal(err)
				}
				if p := sendapitest.CheckTenancy(b.stub.Requests(), userB); len(p) != 0 {
					t.Fatalf("tenancy of the second tenant: %v", p)
				}
			})
		}
	}
}

// Both calls are idempotent at the module (a repeat answers the same and writes nothing), so the client repeats the answer it is given and retries a lost one like a read: three identical
// answers are three identical results, and one 503 then the answer is one logical call. The control is a 400, which is the module's refusal: typed, asked once, never retried.
func TestPauseAndResumeAreIdempotentAndRetriedLikeAReadHandWritten(t *testing.T) {
	for name, p := range profiles() {
		for _, v := range verbs {
			t.Run(name+"/"+v.name, func(t *testing.T) {
				e := newEnv(t, p, userA, reply(200, holdBody(p, campaign, v.heldAfter)), modclient.Options{}, sendapi.Options{})
				first, err := v.call(e.c, t)
				if err != nil {
					t.Fatal(err)
				}
				for i := 0; i < 2; i++ {
					again, err := v.call(e.c, t)
					if err != nil || again != first {
						t.Fatalf("repeat %d: %+v %v, want %+v", i+1, again, err, first)
					}
				}
				if e.stub.Count() != 3 {
					t.Fatalf("%d request(s) for three calls", e.stub.Count())
				}

				r := newEnv(t, p, userA, func(w http.ResponseWriter, _ *http.Request, n int) {
					if n == 1 {
						sendapitest.JSON(w, 503, sendapitest.Envelope("unavailable"))
						return
					}
					sendapitest.JSON(w, 200, holdBody(p, campaign, v.heldAfter))
				}, modclient.Options{Retries: 2}, sendapi.Options{})
				if got, err := v.call(r.c, t); err != nil || got != first || r.stub.Count() != 2 {
					t.Fatalf("a lost answer was not retried: %+v %v after %d request(s)", got, err, r.stub.Count())
				}

				c := newEnv(t, p, userA, reply(400, sendapitest.Envelope("validation_failed", [2]string{"campaign_id", "required"})), modclient.Options{Retries: 2}, sendapi.Options{})
				got, err := v.call(c.c, t)
				ce := wantKind(t, err, modclient.Refused)
				if !isZero(got) || ce.Code != "validation_failed" || c.stub.Count() != 1 {
					t.Fatalf("the control (a refusal): %+v %v after %d request(s)", got, err, c.stub.Count())
				}
			})
		}
	}
}

// Only `campaign_id` and `paused` are relied on, and they are checked before they are believed: a missing, null or non-boolean `paused`, a missing campaign id and the answer for ANOTHER campaign
// are each a Contract error naming the key, with no value. The control is the same body unmutated.
func TestPauseAndResumeRefuseAnAnswerThatIsNotAPauseHandWritten(t *testing.T) {
	for name, p := range profiles() {
		good := holdBody(p, campaign, true)
		cases := map[string]struct{ body, key string }{
			"no paused":                  {`{"campaign_id":"` + campaign + `","state":"paused","changed":true}`, "paused"},
			"a null paused":              {`{"campaign_id":"` + campaign + `","state":"paused","paused":null}`, "paused"},
			"a paused that is a string":  {`{"campaign_id":"` + campaign + `","state":"paused","paused":"true"}`, "paused"},
			"a paused that is a number":  {`{"campaign_id":"` + campaign + `","state":"paused","paused":1}`, "paused"},
			"no campaign id":             {`{"state":"paused","paused":true}`, "campaign_id"},
			"the answer for another one": {holdBody(p, userB, true), "campaign_id"},
		}
		for _, v := range verbs {
			for cname, tc := range cases {
				t.Run(name+"/"+v.name+"/"+cname, func(t *testing.T) {
					e := newEnv(t, p, userA, reply(200, tc.body), modclient.Options{}, sendapi.Options{})
					got, err := v.call(e.c, t)
					if ce := wantKind(t, err, modclient.Contract); ce.Key != tc.key || !isZero(got) {
						t.Fatalf("key %q, result %+v, want key %q and no value", ce.Key, got, tc.key)
					}
				})
			}
			t.Run(name+"/"+v.name+"/control: the good body", func(t *testing.T) {
				e := newEnv(t, p, userA, reply(200, good), modclient.Options{}, sendapi.Options{})
				if got, err := v.call(e.c, t); err != nil || !got.Paused {
					t.Fatalf("the control: the good body is refused: %+v %v", got, err)
				}
			})
		}
	}
}

// The `state` words are the modules' own (module 2: paused, empty, complete, cancelled, running; module 3: running, paused, cancelled, finished, unknown) and module 2's `changed` is its own
// bookkeeping: none is read, so a word the adapter has never heard of, a `changed` of the wrong type and keys nobody documented change nothing. The control flips `paused` in the same bodies and the result
// flips with it.
func TestPauseAndResumeReadOnlyPausedHandWritten(t *testing.T) {
	for name, p := range profiles() {
		for _, v := range verbs {
			t.Run(name+"/"+v.name, func(t *testing.T) {
				for _, held := range []bool{true, false} {
					for _, state := range []string{"teleported", "cancelled", "complete", "empty", "finished", "unknown", ""} {
						body := fmt.Sprintf(`{"campaign_id":%q,"paused":%t,"state":%q,"changed":"maybe","queued":"many","an_undocumented_key":{"x":1}}`, campaign, held, state)
						e := newEnv(t, p, userA, reply(200, body), modclient.Options{}, sendapi.Options{})
						got, err := v.call(e.c, t)
						if err != nil || got.Paused != held || got.CampaignID != campaign {
							t.Fatalf("state %q, paused %t: %+v %v", state, held, got, err)
						}
					}
				}
				// no `state` at all is just as good
				e := newEnv(t, p, userA, reply(200, `{"campaign_id":"`+campaign+`","paused":true}`), modclient.Options{}, sendapi.Options{})
				if got, err := v.call(e.c, t); err != nil || !got.Paused {
					t.Fatalf("a body of the two required keys: %+v %v", got, err)
				}
			})
		}
	}
}

// Errors are classified through the same path as Cancel's: for each answer a module can give, a Pause and a Resume come back with exactly the kind, status and code a Cancel gets from the same
// answer, and with the kind the platform's wrappers expect (a 401 is a refusal that is the platform's configuration, a 404 is NotFound, a 503 `paused` is the module's own pause, a 503
// `unavailable` is Unavailable, a 500 is Internal and a slow module is a Timeout). On every error the result is the zero value. The control is that the kinds differ from one another across the
// table, so the comparison cannot pass by returning one kind for everything.
func TestPauseAndResumeClassifyErrorsLikeCancel(t *testing.T) {
	type answer struct {
		name   string
		status int
		body   string
		delay  time.Duration
		kind   modclient.Kind
		code   string
	}
	answers := []answer{
		{"unauthenticated", 401, sendapitest.Envelope("unauthenticated"), 0, modclient.Refused, "unauthenticated"},
		{"validation_failed", 400, sendapitest.Envelope("validation_failed", [2]string{"campaign_id", "malformed"}), 0, modclient.Refused, "validation_failed"},
		{"not_found", 404, sendapitest.Envelope("not_found"), 0, modclient.NotFound, "not_found"},
		{"paused", 503, sendapitest.Envelope("paused"), 0, modclient.Paused, "paused"},
		{"unavailable", 503, sendapitest.Envelope("unavailable"), 0, modclient.Unavailable, "unavailable"},
		{"internal_error", 500, sendapitest.Envelope("internal_error"), 0, modclient.Internal, "internal_error"},
		{"a slow module", 200, holdBody(m3like(), campaign, true), 400 * time.Millisecond, modclient.Timeout, ""},
	}
	kinds := map[modclient.Kind]bool{}
	for name, p := range profiles() {
		for _, a := range answers {
			kinds[a.kind] = true
			h := func(w http.ResponseWriter, _ *http.Request, _ int) {
				time.Sleep(a.delay)
				sendapitest.JSON(w, a.status, a.body)
			}
			mopts := modclient.Options{}
			if a.delay > 0 {
				mopts.Timeout = 100 * time.Millisecond
			}
			cancelEnv := newEnv(t, p, userA, h, mopts, sendapi.Options{})
			_, cancelErr := cancelEnv.c.Cancel(ctxT(t), campaign)
			cancelErrKind := wantKind(t, cancelErr, a.kind)
			for _, v := range verbs {
				t.Run(name+"/"+a.name+"/"+v.name, func(t *testing.T) {
					e := newEnv(t, p, userA, h, mopts, sendapi.Options{})
					got, err := v.call(e.c, t)
					ce := wantKind(t, err, a.kind)
					if !isZero(got) {
						t.Fatalf("a result came back with the error: %+v", got)
					}
					if ce.Status != cancelErrKind.Status || ce.Code != cancelErrKind.Code || ce.Kind != cancelErrKind.Kind || (a.code != "" && ce.Code != a.code) {
						t.Fatalf("%s is {%s %d %q}, cancel's is {%s %d %q}", v.name, ce.Kind, ce.Status, ce.Code, cancelErrKind.Kind, cancelErrKind.Status, cancelErrKind.Code)
					}
				})
			}
		}
	}
	if len(kinds) < 6 {
		t.Fatalf("the table covers %d kinds: the comparison could pass by returning one kind for everything", len(kinds))
	}
}

// Before anything is sent: a campaign id that is not a lower-case uuid is Invalid and nothing goes out, and a profile with no pause capability is NotSupported with nothing sent. The control is the
// same call with a good id on a profile that has the routes.
func TestPauseAndResumeRefuseBeforeAnyNetworkCall(t *testing.T) {
	for _, v := range verbs {
		t.Run(v.name, func(t *testing.T) {
			e := newEnv(t, m3like(), userA, reply(200, holdBody(m3like(), campaign, true)), modclient.Options{}, sendapi.Options{})
			for _, bad := range []string{"", "not-a-uuid", strings.ToUpper(campaign)} {
				got, err := func() (sendapi.PauseResult, error) {
					if v.name == "pause" {
						return e.c.Pause(ctxT(t), bad)
					}
					return e.c.Resume(ctxT(t), bad)
				}()
				if modclient.KindOf(err) != modclient.Invalid || !isZero(got) {
					t.Fatalf("campaign id %q: %+v %v", bad, got, err)
				}
			}
			if e.stub.Count() != 0 {
				t.Fatalf("%d request(s) for bad ids", e.stub.Count())
			}
			none := m3like()
			none.PauseCapability = ""
			n := newEnv(t, none, userA, reply(200, holdBody(none, campaign, true)), modclient.Options{}, sendapi.Options{})
			got, err := v.call(n.c, t)
			wantKind(t, err, modclient.NotSupported)
			if !isZero(got) || n.stub.Count() != 0 || !errors.Is(err, modclient.ErrNotSupported) {
				t.Fatalf("a profile without the routes: %+v %v after %d request(s)", got, err, n.stub.Count())
			}
			if got, err := v.call(e.c, t); err != nil || e.stub.Count() != 1 || !got.Paused {
				t.Fatalf("the control: a good id on a profile with the routes: %+v %v after %d request(s)", got, err, e.stub.Count())
			}
		})
	}
	// a capability name that is not a word is refused when the client is built, like every other profile fault
	bad := m3like()
	bad.PauseCapability = "Pause Resume!"
	stub := sendapitest.NewStub(t, func(w http.ResponseWriter, _ *http.Request, _ int) { sendapitest.JSON(w, 500, "{}") })
	mc := sendapitest.ModClient(t, modclient.M3, stub.URL(), sendapitest.Token(t), modclient.Options{})
	if _, err := sendapi.New(mc, bad, userA, sendapi.Options{}); modclient.KindOf(err) != modclient.Invalid {
		t.Fatalf("a profile whose pause capability is not a lower-case word was accepted: %v", err)
	}
	if _, err := sendapi.New(mc, m3like(), userA, sendapi.Options{}); err != nil {
		t.Fatalf("the control: %v", err)
	}
}

// ---- the report's `paused` ---------------------------------------------------------------------------------------------------

// withHeader is the campaign of n sent items whose report header is changed by fn (a key added, one turned into the wrong type).
func withHeader(p sendapi.Profile, n int, fn func(h map[string]any)) sendapitest.Campaign {
	c := fakeCampaign(p, userA, n, false)
	inner := c.Header
	c.Header = func(total int) map[string]any {
		h := inner(total)
		fn(h)
		return h
	}
	return c
}

// The campaign report's additive `paused` is THIS campaign's own pause and is independent of `dispatch_paused` (the operator's rung): present true, present false and absent (a module older than
// the change, and every recorded fixture) read true, false and false, on a page and on the whole report, and the two flags never colour each other. A present value that is not a boolean is a
// Contract error naming the key. The controls are the table's own opposite rows (true beside a false dispatch flag and false beside a true one).
func TestCampaignReportCarriesTheCampaignsOwnPauseHandWritten(t *testing.T) {
	for name, p := range profiles() {
		for _, tc := range []struct {
			name       string
			paused     any // nil with absent true means "no key"
			absent     bool
			dispatch   bool
			wantPaused bool
		}{
			{"paused, the operator's rung serving", true, false, false, true},
			{"not paused, the operator's rung serving", false, false, false, false},
			{"no key (a module older than the change), the rung serving", nil, true, false, false},
			{"paused AND the operator's rung halted", true, false, true, true},
			{"not paused but the operator's rung halted", false, false, true, false},
			{"no key but the operator's rung halted", nil, true, true, false},
		} {
			t.Run(name+"/"+tc.name, func(t *testing.T) {
				c := withHeader(p, 2, func(h map[string]any) {
					h["dispatch_paused"] = tc.dispatch
					if !tc.absent {
						h["paused"] = tc.paused
					}
				})
				e := newEnv(t, p, userA, c.Handler(), modclient.Options{}, sendapi.Options{})
				page, err := e.c.ReportCampaignPage(ctxT(t), campaign, "")
				if err != nil || page.Paused != tc.wantPaused || page.DispatchPaused != tc.dispatch {
					t.Fatalf("page: paused=%t dispatch=%t %v, want paused=%t dispatch=%t", page.Paused, page.DispatchPaused, err, tc.wantPaused, tc.dispatch)
				}
				rep, err := e.c.ReportCampaign(ctxT(t), campaign)
				if err != nil || rep.Paused != tc.wantPaused || rep.DispatchPaused != tc.dispatch || len(rep.Items) != 2 {
					t.Fatalf("report: paused=%t dispatch=%t items=%d %v", rep.Paused, rep.DispatchPaused, len(rep.Items), err)
				}
			})
		}
		for cname, v := range map[string]any{"a string": "true", "a number": 1, "null": nil, "an object": map[string]any{"on": true}} {
			t.Run(name+"/paused is "+cname, func(t *testing.T) {
				c := withHeader(p, 2, func(h map[string]any) { h["paused"] = v })
				e := newEnv(t, p, userA, c.Handler(), modclient.Options{}, sendapi.Options{})
				page, err := e.c.ReportCampaignPage(ctxT(t), campaign, "")
				if ce := wantKind(t, err, modclient.Contract); ce.Key != "paused" || len(page.Items) != 0 || page.Paused {
					t.Fatalf("key %q, page %+v", ce.Key, page)
				}
			})
		}
	}
}

// A report of several pages says the pause of the LAST page it read, as it does for its totals (every page is read at a different instant): the campaign paused between two pages reads paused,
// and the one resumed between them reads released. The control is the reverse order, which must say the opposite.
func TestCampaignReportSaysThePauseOfItsLastPageHandWritten(t *testing.T) {
	for name, p := range profiles() {
		for _, tc := range []struct {
			name        string
			first, last bool
		}{{"paused during the walk", false, true}, {"resumed during the walk", true, false}} {
			t.Run(name+"/"+tc.name, func(t *testing.T) {
				calls := 0
				c := withHeader(p, 2, func(h map[string]any) {
					calls++
					h["paused"] = tc.first
					if calls > 1 {
						h["paused"] = tc.last
					}
				})
				e := newEnv(t, p, userA, c.Handler(), modclient.Options{}, sendapi.Options{PageSize: 1})
				rep, err := e.c.ReportCampaign(ctxT(t), campaign)
				if err != nil || rep.Pages != 2 || rep.Paused != tc.last {
					t.Fatalf("pages=%d paused=%t %v, want 2 pages and the last page's paused=%t", rep.Pages, rep.Paused, err, tc.last)
				}
			})
		}
	}
}

// Every recorded page of the OLD modules has no `paused` and still decodes, as not paused; the same recorded page with the key added reads as the key says. The control is the recording untouched.
func TestARecordedReportWithoutPausedIsNotPaused(t *testing.T) {
	for _, name := range []string{"report.campaign.page.json", "report.campaign.last_page.json", "report.campaign.empty.json"} {
		body := sendapitest.Read(t, fixtures, name)
		e := newEnv(t, m3like(), userA, serve(200, body), modclient.Options{}, sendapi.Options{})
		page, err := e.c.ReportCampaignPage(ctxT(t), campaign, "")
		if err != nil || page.Paused {
			t.Fatalf("%s: paused=%t %v", name, page.Paused, err)
		}
		if strings.Contains(string(body), `"paused"`) {
			t.Fatalf("%s already has a paused key: it is not a recording of an old module and the test proves nothing", name)
		}
		with := editJSON(t, body, func(m map[string]any) { m["paused"] = true })
		e = newEnv(t, m3like(), userA, serve(200, with), modclient.Options{}, sendapi.Options{})
		if page, err := e.c.ReportCampaignPage(ctxT(t), campaign, ""); err != nil || !page.Paused {
			t.Fatalf("%s with paused added: paused=%t %v", name, page.Paused, err)
		}
	}
}

// ---- the capability -----------------------------------------------------------------------------------------------------------

// describeWith is the module-3-shaped describe fixture of this package with the module word of the module under test and the capability flags it is given (a key is added or overwritten).
func describeWith(t testing.TB, module string, caps map[string]bool) []byte {
	t.Helper()
	return editJSON(t, sendapitest.Read(t, fixtures, "describe.ok.json"), func(m map[string]any) {
		m["module"] = module
		c := m["capabilities"].(map[string]any)
		for k, v := range caps {
			c[k] = v
		}
	})
}

// The name of the capability is per module (module 2 campaign_pause_resume, module 3 pause_resume) and the answer to "can this module pause one campaign" is the describe's flag under THAT name: true
// when it is true, false when it is false, false when the key is absent (a module older than the change: never an error), and false when only the OTHER module's name is there (the names are not
// interchangeable). A profile with no capability name says false whatever the describe holds, and a nil describe says false. The controls are the true rows.
func TestThePauseCapabilityIsNamedPerModuleHandWritten(t *testing.T) {
	if m2like().PauseCapability != "campaign_pause_resume" || m3like().PauseCapability != "pause_resume" || m2like().PauseCapability == m3like().PauseCapability {
		t.Fatalf("the capability names are %q and %q", m2like().PauseCapability, m3like().PauseCapability)
	}
	for name, tc := range map[string]struct {
		p      sendapi.Profile
		module string
		own    string
		other  string
	}{
		"m2": {m2like(), "send_managed", "campaign_pause_resume", "pause_resume"},
		"m3": {m3like(), "send_byo", "pause_resume", "campaign_pause_resume"},
	} {
		t.Run(name, func(t *testing.T) {
			can := func(body []byte) (bool, error) {
				e := newEnv(t, tc.p, userA, serve(200, body), modclient.Options{}, sendapi.Options{})
				return e.c.PauseSupported(ctxT(t))
			}
			if ok, err := can(describeWith(t, tc.module, map[string]bool{tc.own: true})); err != nil || !ok {
				t.Fatalf("the control (the module's own flag, true): %v %v", ok, err)
			}
			for what, caps := range map[string]map[string]bool{
				"the flag false":                    {tc.own: false},
				"no flag (a module older than C1)":  nil,
				"only the other module's name":      {tc.other: true},
				"the other module's name and false": {tc.other: true, tc.own: false},
			} {
				if ok, err := can(describeWith(t, tc.module, caps)); err != nil || ok {
					t.Fatalf("%s: %v %v, want false and no error", what, ok, err)
				}
			}
			// the describe of another module is a wiring mistake and still the error it always was
			if _, err := can(describeWith(t, "somebody_else", map[string]bool{tc.own: true})); modclient.KindOf(err) != modclient.Contract {
				t.Fatalf("a describe of another module: %v", err)
			}
			// the profile-level answer, with no network
			d := &modclient.Describe{Capabilities: map[string]bool{tc.own: true, "": true}}
			if !tc.p.CanPause(d) || tc.p.CanPause(nil) || tc.p.CanPause(&modclient.Describe{}) {
				t.Fatal("CanPause is not the describe's flag under the profile's name")
			}
			none := tc.p
			none.PauseCapability = ""
			if none.CanPause(d) {
				t.Fatal("a profile with no capability name says a module can pause because a describe holds an empty key")
			}
		})
	}
}

// ---- job_item_cancelled -------------------------------------------------------------------------------------------------------

// A run of an item the platform marked cancelled is answered 409 with the code job_item_cancelled (a code of its own: both senders, Phase 4 D9). It is a typed refusal: errors.Is(err,
// ErrItemCancelled), still the Refused 409 with its code, never back-pressure, never an unexplained conflict, never one of module 3's causes (not even when the body carries details that look
// like causes), asked once even where retries are configured, and no result comes back. The controls: a plain `conflict` 409 keeps its handling (module 2 back-pressure, module 3 unexplained or
// its causes) and is NOT ErrItemCancelled; and the same code on a status other than 409 is not ErrItemCancelled either.
func TestARunOfACancelledItemIsATypedRefusalHandWritten(t *testing.T) {
	cancelledBodies := map[string]string{
		"the bare envelope":                            sendapitest.Envelope("job_item_cancelled"),
		"details that look like a cause of a conflict": sendapitest.Envelope("job_item_cancelled", [2]string{"queue_cap", "conflict"}),
		"a detail naming the item":                     sendapitest.Envelope("job_item_cancelled", [2]string{"job_item_id", "cancelled"}),
	}
	for name, p := range profiles() {
		for bname, body := range cancelledBodies {
			t.Run(name+"/"+bname, func(t *testing.T) {
				e := newEnv(t, p, userA, reply(409, body), modclient.Options{Retries: 2}, sendapi.Options{})
				r, err := e.c.Run(ctxT(t), jobItem)
				ce := wantKind(t, err, modclient.Refused)
				if !isZero(r) || ce.Status != 409 || ce.Code != sendapi.CodeItemCancelled || !errors.Is(err, modclient.ErrRefused) {
					t.Fatalf("the refusal is %+v (result %+v)", ce, r)
				}
				if !errors.Is(err, sendapi.ErrItemCancelled) {
					t.Fatalf("not ErrItemCancelled: %v", err)
				}
				if errors.Is(err, sendapi.ErrBackpressure) || errors.Is(err, sendapi.ErrConflictUnexplained) || len(sendapi.CausesOf(err)) != 0 ||
					errors.Is(err, sendapi.ErrRunQueueCap) || errors.Is(err, sendapi.ErrRunSuspended) || errors.Is(err, sendapi.ErrRunCampaignActive) || errors.Is(err, sendapi.ErrRunCampaignCap) {
					t.Fatalf("a cancelled item was read as a conflict: %v", err)
				}
				if e.stub.Count() != 1 {
					t.Fatalf("%d request(s): a permanent refusal is asked once", e.stub.Count())
				}
			})
		}

		t.Run(name+"/control: a plain conflict is still a conflict", func(t *testing.T) {
			e := newEnv(t, p, userA, reply(409, sendapitest.Envelope("conflict")), modclient.Options{}, sendapi.Options{})
			_, err := e.c.Run(ctxT(t), jobItem)
			wantKind(t, err, modclient.Refused)
			if errors.Is(err, sendapi.ErrItemCancelled) {
				t.Fatal("a plain conflict is read as a cancelled item")
			}
			if p.RunConflict == sendapi.ConflictBackpressure && !errors.Is(err, sendapi.ErrBackpressure) {
				t.Fatalf("module 2's conflict is not back-pressure any more: %v", err)
			}
			if p.RunConflict == sendapi.ConflictCauses && !errors.Is(err, sendapi.ErrConflictUnexplained) {
				t.Fatalf("module 3's conflict with no causes is not unexplained any more: %v", err)
			}
		})
		t.Run(name+"/control: the code on another status is no cancelled item", func(t *testing.T) {
			for _, status := range []int{400, 404, 422, 500} {
				e := newEnv(t, p, userA, reply(status, sendapitest.Envelope(sendapi.CodeItemCancelled)), modclient.Options{}, sendapi.Options{})
				_, err := e.c.Run(ctxT(t), jobItem)
				if err == nil || errors.Is(err, sendapi.ErrItemCancelled) {
					t.Fatalf("status %d: %v", status, err)
				}
			}
		})
	}
	t.Run("m3/control: a conflict that names its causes keeps them", func(t *testing.T) {
		e := newEnv(t, m3like(), userA, reply(409, conflictBody("campaign_active", "queue_cap")), modclient.Options{}, sendapi.Options{})
		_, err := e.c.Run(ctxT(t), jobItem)
		if errors.Is(err, sendapi.ErrItemCancelled) || !slices.Equal(sendapi.CausesOf(err), []sendapi.Cause{sendapi.CauseCampaignActive, sendapi.CauseQueueCap}) {
			t.Fatalf("the causes of a conflict: %v", err)
		}
	})
}

// A describe that lists the error code job_item_cancelled and one that does not are both accepted: neither its absence (a module older than D9) is a missing code nor its presence an extra one, so
// the comparison is neither Breaking nor Additive for it. The controls: an ordinary code the adapter does not know is still reported (Additive), and one it maps that is gone is still Breaking.
func TestADescribeMayOrMayNotListTheCancelledItemCodeHandWritten(t *testing.T) {
	p := m3like()
	good := sendapitest.Read(t, fixtures, "describe.ok.json")
	check := func(body []byte) (sendapi.VocabReport, error) {
		e := newEnv(t, p, userA, serve(200, body), modclient.Options{}, sendapi.Options{})
		return sendapi.CheckVocabularies(ctxT(t), e.c.Module(), p)
	}
	withCode := editJSON(t, good, func(m map[string]any) {
		m["error_codes"] = append(m["error_codes"].([]any), map[string]any{"code": sendapi.CodeItemCancelled, "http_status": 409})
	})
	for name, body := range map[string][]byte{"a describe without the code (older module)": good, "a describe with the code": withCode} {
		rep, err := check(body)
		if err != nil || rep.Breaking() || rep.Additive() || len(rep.CodesMissing) != 0 || len(rep.CodesExtra) != 0 {
			t.Fatalf("%s: %v %+v", name, err, rep)
		}
	}
	if rep, err := check(appendCode(t, withCode, "teapot")); err != nil || rep.Breaking() || !rep.Additive() || !slices.Equal(rep.CodesExtra, []string{"teapot"}) {
		t.Fatalf("the control (an unknown code beside it): %v %+v", err, rep)
	}
	if _, err := check(mutated(t, withCode, "error_codes[8]", sendapitest.Delete, nil)); modclient.KindOf(err) != modclient.Contract {
		t.Fatalf("the control (a mapped code is gone): %v", err)
	}
	got := sendapi.OptionalErrorCodes()
	if !slices.Equal(got, []string{"job_item_cancelled"}) {
		t.Fatalf("the optional codes are %v", got)
	}
	got[0] = "tampered"
	if sendapi.OptionalErrorCodes()[0] != "job_item_cancelled" {
		t.Fatal("OptionalErrorCodes shares its slice")
	}
}
