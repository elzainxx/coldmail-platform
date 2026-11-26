package m2_test

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/adapters/m2"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi/sendapitest"
	"github.com/coldmail-platform/platform/internal/charge"
	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/modclient"
	"github.com/coldmail-platform/platform/internal/tenant"
)

// The tenants and ids of the tests: synthetic, none a secret, none the all-zero system user.
const (
	userA    = "2f6d1c1e-7a4b-4c55-9f0e-5b7a3c9d8e21"
	userB    = "9c1e7a3b-5d2f-4a6c-8b0e-1f3d5a7c9e42"
	campaign = "5a1c9e30-7d2b-4f6a-9b8e-1c3d5e7f9a02"
	jobItem  = "a1b2c3d4-0000-4000-8000-000000000001"
	jobID    = "0d3e5f70-8192-4a3b-8c4d-5e6f708192a3"
	fixtures = "testdata/contract"
)

type handler = func(w http.ResponseWriter, r *http.Request, n int)

func ctxT(t testing.TB) context.Context { return sendapitest.Ctx(t) }

// stack is an adapter for module 2 at a stub: the stub, the tenant-free adapter and the token.
type stack struct {
	stub  *sendapitest.Stub
	a     *m2.Adapter
	mc    *modclient.Client
	token string
}

func newStack(t testing.TB, h handler, mopts modclient.Options) *stack {
	t.Helper()
	if h == nil {
		h = func(w http.ResponseWriter, _ *http.Request, _ int) {
			sendapitest.JSON(w, 500, sendapitest.Envelope("internal_error"))
		}
	}
	stub := sendapitest.NewStub(t, h)
	token := sendapitest.Token(t)
	mc := sendapitest.ModClient(t, modclient.M2, stub.URL(), token, mopts)
	a, err := m2.NewAdapter(mc)
	if err != nil {
		t.Fatal(err)
	}
	return &stack{stub: stub, a: a, mc: mc, token: token}
}

func (s *stack) client(t testing.TB, user string) *m2.Client {
	t.Helper()
	c, err := s.a.For(user)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func reply(status int, body string) handler {
	return func(w http.ResponseWriter, _ *http.Request, _ int) { sendapitest.JSON(w, status, body) }
}

func wantKind(t testing.TB, err error, want modclient.Kind) *modclient.Error {
	t.Helper()
	if err == nil {
		t.Fatalf("no error, want kind %s", want)
	}
	if got := modclient.KindOf(err); got != want {
		t.Fatalf("kind %s, want %s: %v", got, want, err)
	}
	var e *modclient.Error
	if !errors.As(err, &e) {
		t.Fatalf("error %v has no *modclient.Error", err)
	}
	return e
}

func isZero(v any) bool { return v == nil || reflect.ValueOf(v).IsZero() }

// ---- the profile and the construction -----------------------------------------------------------------------------------

// Module 2's profile is what the research documents (research m2 section 4.3, describe, handlers): the seven states in enum order, the six error classes, the twelve reasons in the
// module's contract order, the eight Domain A error codes (job_item_cancelled, the 409 for an item the platform marked cancelled, among them since the pin that lists it in describe), and the conventions that make it module 2 (the single report always carries its optional keys, no account detail, a
// quote that is arithmetic, a 409 that is back-pressure). The control changes one field and the comparison fails.
func TestProfileIsTheDocumentedOne(t *testing.T) {
	want := sendapi.Profile{
		Module: modclient.M2, DescribeModule: "send_managed",
		JobStates:    []string{"queued", "assigned", "sending", "sent", "failed", "bounced", "cancelled"},
		ErrorClasses: []string{"bounce_permanent", "transport_4xx", "auth_failure", "throttle", "timeout", "unknown"},
		ItemReasons: []string{"pool_exhausted", "attempts_exhausted", "bounce_permanent", "cancelled", "transport_4xx", "auth_failure", "throttle", "timeout", "unknown",
			"no_eligible_account", "waiting_for_capacity", "dispatch_paused"},
		ErrorCodes:    []string{"validation_failed", "unauthenticated", "not_found", "method_not_allowed", "conflict", "job_item_cancelled", "internal_error", "paused"},
		AlwaysPresent: true, LinearCost: true, Cancel: true, RunConflict: sendapi.ConflictBackpressure,
		PauseCapability: "campaign_pause_resume",
	}
	if got := m2.Profile(); !reflect.DeepEqual(got, want) {
		t.Fatalf("the profile is not the documented one:\n got %+v\nwant %+v", got, want)
	}
	changed := want
	changed.LinearCost = false
	if reflect.DeepEqual(m2.Profile(), changed) {
		t.Fatal("the control: a different profile compares equal")
	}
	// each call returns a fresh profile: editing one does not edit the next
	p := m2.Profile()
	p.JobStates[0] = "tampered"
	if m2.Profile().JobStates[0] != "queued" {
		t.Fatal("Profile shares its slices between calls")
	}
	// the profile builds a client (it passes the shared body's validation)
	s := newStack(t, nil, modclient.Options{})
	if _, err := s.a.For(userA); err != nil {
		t.Fatal(err)
	}
}

// FromConfig reads PLATFORM_M2_URL and PLATFORM_M2_SERVICE_TOKEN and the shared module-call dials. Either missing is ErrNotConfigured (and a modclient.Invalid error: nothing is sent);
// with both, the adapter calls the configured URL with the configured token and retries an idempotent call as many times as the dial says, and no more. Config.Load is the other end:
// what it yields for the two variables is what FromConfig consumes.
func TestFromConfig(t *testing.T) {
	stub := sendapitest.NewStub(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		sendapitest.JSON(w, 503, sendapitest.Envelope("unavailable"))
	})
	token := sendapitest.Token(t)
	good := config.Config{M2URL: stub.URL(), M2ServiceToken: config.Secret(token), ModuleTimeout: 2 * time.Second, ModuleRetries: 2, ModuleRetryBackoff: 0}
	a, err := m2.FromConfig(good)
	if err != nil {
		t.Fatal(err)
	}
	c, err := a.For(userA)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Run(ctxT(t), jobItem)
	wantKind(t, err, modclient.Unavailable)
	reqs := stub.Requests()
	if len(reqs) != 3 || reqs[0].Auth != "Bearer "+token || reqs[0].Path != "/v1/run" {
		t.Fatalf("%d request(s), first %+v: the dials or the credential did not arrive", len(reqs), reqs[0])
	}
	// the dials are consumed: no retries means one request
	stub2 := sendapitest.NewStub(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		sendapitest.JSON(w, 503, sendapitest.Envelope("unavailable"))
	})
	none := good
	none.M2URL, none.ModuleRetries = stub2.URL(), 0
	a2, err := m2.FromConfig(none)
	if err != nil {
		t.Fatal(err)
	}
	c2, _ := a2.For(userA)
	_, _ = c2.Run(ctxT(t), jobItem)
	if stub2.Count() != 1 {
		t.Fatalf("with no retries %d requests were made", stub2.Count())
	}
	// not configured
	for name, cfg := range map[string]config.Config{
		"nothing":   {},
		"no URL":    {M2ServiceToken: config.Secret(token)},
		"no token":  {M2URL: stub.URL()},
		"neither":   {ModuleRetries: 2},
		"bad URL":   {M2URL: "ftp://x.example", M2ServiceToken: config.Secret(token)},
		"bad token": {M2URL: stub.URL(), M2ServiceToken: config.Secret("has space")},
	} {
		t.Run("refuses "+name, func(t *testing.T) {
			before := stub.Count()
			a, err := m2.FromConfig(cfg)
			if a != nil || err == nil {
				t.Fatalf("an adapter came back: %v", err)
			}
			wantKind(t, err, modclient.Invalid)
			if name != "bad URL" && name != "bad token" && !errors.Is(err, m2.ErrNotConfigured) {
				t.Fatalf("not ErrNotConfigured: %v", err)
			}
			if stub.Count() != before {
				t.Fatal("a refused configuration sent a request")
			}
		})
	}
	// what Load yields for the two variables is what FromConfig consumes
	env := map[string]string{
		config.VarDatabaseURL: "postgres://platform_app@db.example:5432/postgres", config.VarM3URL: "http://m3.example:8080", config.VarM3AdminToken: "operator-operator-ok",
		config.VarM2URL: stub.URL(), config.VarM2ServiceToken: token, config.VarModuleRetries: "1", config.VarModuleRetryBackoffMS: "0",
	}
	cfg, err := config.Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	a3, err := m2.FromConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	stub3 := stub.Count()
	c3, _ := a3.For(userA)
	_, _ = c3.Run(ctxT(t), jobItem)
	if got := stub.Count() - stub3; got != 2 {
		t.Fatalf("PLATFORM_MODULE_RETRIES=1 gave %d requests, want 2", got)
	}
	// unset in the environment is "not configured"
	delete(env, config.VarM2URL)
	delete(env, config.VarM2ServiceToken)
	cfg, err = config.Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m2.FromConfig(cfg); !errors.Is(err, m2.ErrNotConfigured) {
		t.Fatalf("an unset configuration: %v", err)
	}
	// and the string form of the adapter's error never carries the token
	if _, err := m2.FromConfig(config.Config{M2URL: "http://user:" + token + "@x.example", M2ServiceToken: config.Secret(token)}); err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("a credential in the URL: %v", err)
	}
}

func TestNewAdapterRefusesAnotherModulesClient(t *testing.T) {
	stub := sendapitest.NewStub(t, func(w http.ResponseWriter, _ *http.Request, _ int) { sendapitest.JSON(w, 500, "{}") })
	for name, mc := range map[string]*modclient.Client{
		"module 3": sendapitest.ModClient(t, modclient.M3, stub.URL(), sendapitest.Token(t), modclient.Options{}),
		"module 1": sendapitest.ModClient(t, modclient.M1, stub.URL(), sendapitest.Token(t), modclient.Options{}),
		"nil":      nil,
	} {
		a, err := m2.NewAdapter(mc)
		if a != nil {
			t.Fatalf("%s: an adapter came back", name)
		}
		wantKind(t, err, modclient.Invalid)
	}
	if _, err := m2.NewAdapter(sendapitest.ModClient(t, modclient.M2, stub.URL(), sendapitest.Token(t), modclient.Options{})); err != nil {
		t.Fatalf("control: %v", err)
	}
	if stub.Count() != 0 {
		t.Fatal("a constructor sent a request")
	}
}

// For refuses what cannot be a tenant before any network call: the all-zero system user, an empty id, an upper-case or malformed one. The control builds a client for a real tenant.
func TestForRefusesWhatIsNotATenant(t *testing.T) {
	s := newStack(t, nil, modclient.Options{})
	for id, is := range map[string]error{"": tenant.ErrBadID, tenant.SystemUserID: tenant.ErrSystemUser, strings.ToUpper(userA): tenant.ErrBadID, "tenant-1": tenant.ErrBadID} {
		c, err := s.a.For(id)
		if c != nil {
			t.Fatalf("%q: a client came back", id)
		}
		wantKind(t, err, modclient.Invalid)
		if !errors.Is(err, is) {
			t.Fatalf("%q: %v is not %v", id, err, is)
		}
	}
	if s.stub.Count() != 0 {
		t.Fatal("a refused tenant sent a request")
	}
	if _, err := s.a.For(userA); err != nil {
		t.Fatalf("control: %v", err)
	}
}

// ---- tenancy ------------------------------------------------------------------------------------------------------------------

// Every call of module 2's client, cancel included, carries the tenant it was built for (body for quote, run and cancel; query for the reports) and the bearer token. The same check
// run on a client built for another tenant names it.
func TestEveryCallCarriesItsTenant(t *testing.T) {
	good := func(name string) []byte { return sendapitest.Read(t, fixtures, name) }
	pages := campaignPages(t)
	h := func(w http.ResponseWriter, r *http.Request, _ int) {
		switch {
		case r.URL.Path == "/v1/quote":
			sendapitest.JSON(w, 200, string(good("quote.ok.json")))
		case r.URL.Path == "/v1/run":
			sendapitest.JSON(w, 202, string(good("run.created.json")))
		case r.URL.Path == "/v1/cancel":
			sendapitest.JSON(w, 200, string(good("cancel.ok.json")))
		case r.URL.Query().Get("campaign_id") != "":
			pages.follow()(w, r, 0) // a recorded page is not a whole campaign: the stub follows the cursor value
		default:
			sendapitest.JSON(w, 200, string(good("report.job.sent.json")))
		}
	}
	s := newStack(t, h, modclient.Options{})
	c := s.client(t, userA)
	ctx := ctxT(t)
	// the ids and the count the recordings answer for are read from the recordings
	count := atInt(t, "quote.ok.json", "recipient_count")
	reportID := atStr(t, pages.source, "campaign_id")
	cancelID := atStr(t, "cancel.ok.json", "campaign_id")
	if _, err := c.Quote(ctx, count); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Run(ctx, jobItem); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReportJob(ctx, jobItem); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReportCampaign(ctx, reportID); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Cancel(ctx, cancelID); err != nil {
		t.Fatal(err)
	}
	reqs := s.stub.Requests()
	if len(reqs) != 6 { // quote, run, the single report, the two pages of the campaign walk, cancel
		t.Fatalf("%d requests, want 6", len(reqs))
	}
	if problems := sendapitest.CheckTenancy(reqs, userA); len(problems) != 0 {
		t.Fatalf("%v", problems)
	}
	for _, r := range reqs {
		if r.Auth != "Bearer "+s.token {
			t.Fatalf("%s %s: Authorization %q", r.Method, r.Path, r.Auth)
		}
	}
	// control: the same check names a client for another tenant, cancel included
	other := s.client(t, userB)
	before := len(s.stub.Requests())
	if _, err := other.Cancel(ctx, cancelID); err != nil {
		t.Fatal(err)
	}
	if problems := sendapitest.CheckTenancy(s.stub.Requests()[before:], userA); len(problems) != 1 || !strings.Contains(problems[0], userB) {
		t.Fatalf("the check did not name tenant B's cancel: %v", problems)
	}
}

// ---- cancel -------------------------------------------------------------------------------------------------------------------

// Cancel is a position, not a delta: a second call answers the same numbers, the partition (cancelled, in flight, already terminal) is what the caller asserts and not the split, and a
// cancel the module answers with all zeros is Empty (an unknown or foreign campaign cannot be told from nothing to cancel). The recorded answer is read for what it says (the real
// module answered {0, 0, 0} to the recording run's cancel); a position with something in it is that recording with its counts filled in, since the real run did not catch one.
func TestCancelIsAPositionAndASecondCancelIsANoOp(t *testing.T) {
	id := atStr(t, "cancel.ok.json", "campaign_id")
	recorded := string(sendapitest.Read(t, fixtures, "cancel.ok.json"))
	filled := recorded
	for path, n := range map[string]int{"cancelled": 2, "in_flight": 1, "already_terminal": 0} {
		b, err := sendapitest.Mutate([]byte(filled), path, sendapitest.Set, n)
		if err != nil {
			t.Fatal(err)
		}
		filled = string(b)
	}
	post := func(body string) handler {
		return func(w http.ResponseWriter, r *http.Request, _ int) {
			if r.Method != http.MethodPost {
				sendapitest.JSON(w, 405, sendapitest.Envelope("method_not_allowed"))
				return
			}
			sendapitest.JSON(w, 200, body)
		}
	}
	s := newStack(t, post(filled), modclient.Options{})
	c := s.client(t, userA)
	first, err := c.Cancel(ctxT(t), id)
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.Cancel(ctxT(t), id)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || first.Cancelled != 2 || first.InFlight != 1 || first.AlreadyTerminal != 0 || first.CampaignID != id || first.Empty() {
		t.Fatalf("first %+v second %+v", first, second)
	}
	reqs := s.stub.Requests()
	if len(reqs) != 2 || reqs[0].Method != http.MethodPost || reqs[0].Path != "/v1/cancel" {
		t.Fatalf("requests: %+v", reqs)
	}
	// the recorded answer as it is: Empty exactly when the file says all zeros
	zero := newStack(t, post(recorded), modclient.Options{})
	p, err := zero.client(t, userA).Cancel(ctxT(t), id)
	wantEmpty := atInt(t, "cancel.ok.json", "cancelled") == 0 && atInt(t, "cancel.ok.json", "in_flight") == 0 && atInt(t, "cancel.ok.json", "already_terminal") == 0
	if err != nil || p.Empty() != wantEmpty || p.CampaignID != id || p.Cancelled != atInt(t, "cancel.ok.json", "cancelled") {
		t.Fatalf("the recorded answer: %v %+v", err, p)
	}
	for _, partial := range []string{`"cancelled":1,"in_flight":0,"already_terminal":0`, `"cancelled":0,"in_flight":1,"already_terminal":0`, `"cancelled":0,"in_flight":0,"already_terminal":1`} {
		e := newStack(t, reply(200, `{"campaign_id":"`+campaign+`",`+partial+`}`), modclient.Options{})
		if p, err := e.client(t, userA).Cancel(ctxT(t), campaign); err != nil || p.Empty() {
			t.Fatalf("%s: %v %+v", partial, err, p)
		}
	}
}

// What cancel refuses before it sends (an id that is not a uuid) and what it refuses after (an answer for another campaign, a negative or missing count, a count of the wrong type);
// every failure of the module is a typed error with the zero position. The control is the good answer.
func TestCancelRefusesWhatIsNotAnAnswer(t *testing.T) {
	good := `{"campaign_id":"` + campaign + `","cancelled":2,"in_flight":1,"already_terminal":0}`
	s := newStack(t, reply(200, good), modclient.Options{})
	c := s.client(t, userA)
	for _, bad := range []string{"", "x", strings.ToUpper(campaign)} {
		p, err := c.Cancel(ctxT(t), bad)
		wantKind(t, err, modclient.Invalid)
		if !isZero(p) {
			t.Fatalf("a position came back: %+v", p)
		}
	}
	if s.stub.Count() != 0 {
		t.Fatal("a bad campaign id was sent")
	}
	if p, err := c.Cancel(ctxT(t), campaign); err != nil || p.Cancelled != 2 {
		t.Fatalf("control: %v %+v", err, p)
	}
	for name, tc := range map[string]struct{ body, key string }{
		"another campaign":        {`{"campaign_id":"5a1c9e30-7d2b-4f6a-9b8e-1c3d5e7f9a99","cancelled":2,"in_flight":1,"already_terminal":0}`, "campaign_id"},
		"negative cancelled":      {`{"campaign_id":"` + campaign + `","cancelled":-1,"in_flight":1,"already_terminal":0}`, "cancelled"},
		"negative in_flight":      {`{"campaign_id":"` + campaign + `","cancelled":2,"in_flight":-1,"already_terminal":0}`, "in_flight"},
		"negative terminal":       {`{"campaign_id":"` + campaign + `","cancelled":2,"in_flight":1,"already_terminal":-5}`, "already_terminal"},
		"missing in_flight":       {`{"campaign_id":"` + campaign + `","cancelled":2,"already_terminal":0}`, "in_flight"},
		"cancelled is text":       {`{"campaign_id":"` + campaign + `","cancelled":"2","in_flight":1,"already_terminal":0}`, "cancelled"},
		"cancelled is fractional": {`{"campaign_id":"` + campaign + `","cancelled":2.5,"in_flight":1,"already_terminal":0}`, "cancelled"},
		"an empty object":         {`{}`, "campaign_id"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newStack(t, reply(200, tc.body), modclient.Options{})
			p, err := e.client(t, userA).Cancel(ctxT(t), campaign)
			if ce := wantKind(t, err, modclient.Contract); ce.Key != tc.key {
				t.Fatalf("key %q, want %q: %v", ce.Key, tc.key, err)
			}
			if !isZero(p) {
				t.Fatalf("a position came back with the error: %+v", p)
			}
		})
	}
	for name, tc := range map[string]struct {
		h    handler
		kind modclient.Kind
	}{
		"module fault":  {reply(500, sendapitest.Envelope("internal_error")), modclient.Internal},
		"paused":        {reply(503, sendapitest.Envelope("paused")), modclient.Paused},
		"unavailable":   {reply(503, sendapitest.Envelope("unavailable")), modclient.Unavailable},
		"bad request":   {reply(400, sendapitest.Envelope("validation_failed", [2]string{"campaign_id", "required"})), modclient.Refused},
		"wrong service": {reply(401, sendapitest.Envelope("unauthenticated")), modclient.Refused},
		"not json":      {reply(200, `<html>`), modclient.Contract},
		"truncated":     {reply(200, `{"campaign_id":"`), modclient.Contract},
	} {
		t.Run(name, func(t *testing.T) {
			e := newStack(t, tc.h, modclient.Options{})
			p, err := e.client(t, userA).Cancel(ctxT(t), campaign)
			wantKind(t, err, tc.kind)
			if !isZero(p) {
				t.Fatalf("a position came back with the error: %+v", p)
			}
		})
	}
	down := newStack(t, reply(200, good), modclient.Options{})
	down.stub.Close()
	if p, err := down.client(t, userA).Cancel(ctxT(t), campaign); modclient.KindOf(err) != modclient.Unavailable || !isZero(p) {
		t.Fatalf("a module that is down: %v %+v", err, p)
	}
}

// Cancel is retried like a read where nothing was confirmed (a second cancel changes nothing) and never after the module answered with a refusal or a fault.
func TestCancelIsRetriedOnlyWhereNothingWasConfirmed(t *testing.T) {
	down := newStack(t, reply(503, sendapitest.Envelope("unavailable")), modclient.Options{Retries: 2})
	if _, err := down.client(t, userA).Cancel(ctxT(t), campaign); modclient.KindOf(err) != modclient.Unavailable || down.stub.Count() != 3 {
		t.Fatalf("%v after %d requests, want 3", err, down.stub.Count())
	}
	for _, status := range []int{400, 401, 500} {
		code := map[int]string{400: "validation_failed", 401: "unauthenticated", 500: "internal_error"}[status]
		s := newStack(t, reply(status, sendapitest.Envelope(code)), modclient.Options{Retries: 2})
		_, _ = s.client(t, userA).Cancel(ctxT(t), campaign)
		if s.stub.Count() != 1 {
			t.Fatalf("a %d was retried: %d requests", status, s.stub.Count())
		}
	}
}

// Cancelled items are terminal and free: charge rule M2-08, cited by id. In flight items keep going: while they are assigned or sending the adapter says "not yet", and when they finish
// they are settled once, under the same item_ref. Nothing here is Unmapped.
func TestCancelledItemsAreFreeAndInFlightItemsFinishOnce(t *testing.T) {
	d := charge.Decide(charge.Outcome{Module: "m2", State: "cancelled", Reason: "cancelled"})
	if d.Kind != charge.Free || d.Rule != "M2-08" {
		t.Fatalf("the table says %s by %s for a cancelled item, want free by M2-08", d.Kind, d.Rule)
	}
	reason := func(s string) *string { return &s }
	s := newStack(t, nil, modclient.Options{})
	c := s.client(t, userA)
	// the report just after a cancel: one item already cancelled, one still sending, one sent before the cancel
	after := []sendapi.Item{
		{JobItemID: "a1b2c3d4-0000-4000-8000-000000000001", State: "cancelled", Reason: reason("cancelled")},
		{JobItemID: "a1b2c3d4-0000-4000-8000-000000000002", State: "sending"},
		{JobItemID: "a1b2c3d4-0000-4000-8000-000000000003", State: "sent"},
	}
	first, err := c.Classify(after)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Settle) != 2 || len(first.NotYet) != 1 || len(first.Unmapped) != 0 || first.NotYet[0].State != "sending" {
		t.Fatalf("after the cancel: %+v", first)
	}
	if got := charge.Decide(first.Settle[0].Outcome); got.Rule != "M2-08" || got.Kind != charge.Free {
		t.Fatalf("the cancelled item is decided by %s as %s", got.Rule, got.Kind)
	}
	// the in-flight item finishes (sent): it appears once, with the item_ref it had, and the two earlier ones are the same items again (the ledger's own key keeps them settled once)
	finished := append([]sendapi.Item(nil), after...)
	finished[1] = sendapi.Item{JobItemID: after[1].JobItemID, State: "sent"}
	second, err := c.Classify(finished)
	if err != nil || len(second.Settle) != 3 || len(second.NotYet) != 0 {
		t.Fatalf("after the in-flight item finished: %v %+v", err, second)
	}
	refs := map[string]int{}
	for _, it := range second.Settle {
		refs[it.ItemRef]++
	}
	if len(refs) != 3 || refs[after[1].JobItemID] != 1 {
		t.Fatalf("item refs: %v", refs)
	}
	// and an in-flight item whose attempt FAILS after the cancel ends cancelled, not failed: still free, still not Unmapped
	third, err := c.Classify([]sendapi.Item{{JobItemID: after[1].JobItemID, State: "cancelled", Reason: reason("cancelled")}})
	if err != nil || len(third.Settle) != 1 || charge.Decide(third.Settle[0].Outcome).Rule != "M2-08" {
		t.Fatalf("%v %+v", err, third)
	}
}

// ---- the poll cadence ---------------------------------------------------------------------------------------------------------

// The cadence constants are the research's numbers (a campaign every 2 to 5 seconds, pages of 200, the module's 1 second dispatch tick) and relate to each other the way the guidance
// says; the page size is the shared body's maximum, so a walk at the documented size is accepted by the client.
func TestPollCadenceIsTheDocumentedGuidance(t *testing.T) {
	if m2.PollIntervalMin != 2*time.Second || m2.PollIntervalMax != 5*time.Second || m2.DispatchTick != time.Second {
		t.Fatalf("the cadence is %v, %v, %v", m2.PollIntervalMin, m2.PollIntervalMax, m2.DispatchTick)
	}
	if !(m2.PollIntervalMin <= m2.PollInterval && m2.PollInterval <= m2.PollIntervalMax) || m2.PollIntervalMin < m2.DispatchTick {
		t.Fatalf("the default %v is not between %v and %v, or is faster than the dispatch tick", m2.PollInterval, m2.PollIntervalMin, m2.PollIntervalMax)
	}
	size, maxSize := int(m2.ReportPageSize), int(sendapi.MaxPageSize)
	if size != 200 || size != maxSize {
		t.Fatalf("the report page size is %d", size)
	}
	pages := campaignPages(t)
	s := newStack(t, pages.follow(), modclient.Options{})
	if _, err := s.client(t, userA).ReportCampaign(ctxT(t), atStr(t, pages.source, "campaign_id")); err != nil {
		t.Fatal(err)
	}
	if got := s.stub.Requests()[0].Query.Get("page_size"); got != "200" {
		t.Fatalf("the client asks for page_size %q, the documented size is 200", got)
	}
}
