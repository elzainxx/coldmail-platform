package sendapi_test

import (
	"errors"
	"math"
	"net/http"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi/sendapitest"
	"github.com/coldmail-platform/platform/internal/modclient"
	"github.com/coldmail-platform/platform/internal/tenant"
)

// ---- construction and tenancy ------------------------------------------------------------------------------------------------

// A client that cannot name its tenant refuses before any network call: an empty id, the all-zero system user, a spelling that is not the canonical one, the wrong module client, an
// invalid profile and options out of range, each with its own reason. The control builds a client from the same pieces with a good tenant.
func TestNewRefusesBeforeAnyNetworkCall(t *testing.T) {
	stub := sendapitest.NewStub(t, func(w http.ResponseWriter, _ *http.Request, _ int) { sendapitest.JSON(w, 500, "{}") })
	mc := sendapitest.ModClient(t, modclient.M2, stub.URL(), sendapitest.Token(t), modclient.Options{})
	m3mc := sendapitest.ModClient(t, modclient.M3, stub.URL(), sendapitest.Token(t), modclient.Options{})
	badProfile := m2like()
	badProfile.JobStates = append(badProfile.JobStates, "paused") // a state the adapter's switch does not handle
	noTerminal := m2like()
	noTerminal.JobStates = []string{"queued"}
	dup := m2like()
	dup.ErrorClasses = []string{"timeout", "timeout"}
	noConflict := m2like()
	noConflict.RunConflict = 0

	cases := []struct {
		name string
		mc   *modclient.Client
		p    sendapi.Profile
		user string
		opts sendapi.Options
		is   error
		why  string
	}{
		{"empty tenant", mc, m2like(), "", sendapi.Options{}, tenant.ErrBadID, "empty"},
		{"all-zero tenant", mc, m2like(), tenant.SystemUserID, sendapi.Options{}, tenant.ErrSystemUser, "system user"},
		{"upper-case tenant", mc, m2like(), strings.ToUpper(userA), sendapi.Options{}, tenant.ErrBadID, "lower-case uuid"},
		{"not a uuid", mc, m2like(), "tenant-1", sendapi.Options{}, tenant.ErrBadID, "lower-case uuid"},
		{"no module client", nil, m2like(), userA, sendapi.Options{}, nil, "no module client"},
		{"module 3 client with module 2 profile", m3mc, m2like(), userA, sendapi.Options{}, nil, "another module"},
		{"a state the switch does not handle", mc, badProfile, userA, sendapi.Options{}, nil, "does not handle"},
		{"no terminal state", mc, noTerminal, userA, sendapi.Options{}, nil, "no terminal state"},
		{"a set that lists a value twice", mc, dup, userA, sendapi.Options{}, nil, "twice"},
		{"no meaning for a 409", mc, noConflict, userA, sendapi.Options{}, nil, "409"},
		{"page size above the module's maximum", mc, m2like(), userA, sendapi.Options{PageSize: sendapi.MaxPageSize + 1}, nil, "page size"},
		{"negative page limit", mc, m2like(), userA, sendapi.Options{MaxPages: -1}, nil, "page limit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := sendapi.New(tc.mc, tc.p, tc.user, tc.opts)
			if c != nil {
				t.Fatal("a client came back with an error")
			}
			e := wantKind(t, err, modclient.Invalid)
			if !strings.Contains(e.Message, tc.why) {
				t.Fatalf("the reason %q does not say %q", e.Message, tc.why)
			}
			if tc.is != nil && !errors.Is(err, tc.is) {
				t.Fatalf("errors.Is(err, %v) = false: %v", tc.is, err)
			}
		})
	}
	if stub.Count() != 0 {
		t.Fatalf("%d request(s) were sent by constructors that refuse", stub.Count())
	}
	// control: the same pieces with a good tenant build a client, so the refusals above are about what they name
	if _, err := sendapi.New(mc, m2like(), userA, sendapi.Options{PageSize: 50, MaxPages: 3}); err != nil {
		t.Fatalf("a good tenant is refused: %v", err)
	}
}

// Every request of every route carries the user_id the client was built for: in the body for quote and run, in the query for the reports; and the bearer token. The paired control runs the
// SAME check on the requests of a client built for another tenant and on a hand-made request with the wrong user: it must name them.
func TestEveryRequestCarriesTheUserItWasBuiltFor(t *testing.T) {
	for name, p := range profiles() {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, p, userA, func(w http.ResponseWriter, r *http.Request, _ int) {
				switch {
				case r.URL.Path == "/v1/quote":
					sendapitest.JSON(w, 200, `{"recipient_count":3,"cost":0,"currency":"credits","unit_price_credits":0,"tier":"shared","feasible":true,"reasons":[]}`)
				case r.URL.Path == "/v1/run":
					sendapitest.JSON(w, 202, `{"job_id":"`+jobID+`","state":"queued"}`)
				case r.URL.Query().Get("campaign_id") != "":
					fakeCampaign(p, userA, 3, false).Handler()(w, r, 0)
				default:
					sendapitest.JSON(w, 200, `{"job_id":"`+jobID+`","state":"queued","attempt_count":0,"reason":null,"error_class":null}`)
				}
			}, modclient.Options{}, sendapi.Options{PageSize: 2})
			ctx := ctxT(t)
			if _, err := e.c.Quote(ctx, 3); err != nil {
				t.Fatal(err)
			}
			if _, err := e.c.Run(ctx, jobItem); err != nil {
				t.Fatal(err)
			}
			if _, err := e.c.ReportJob(ctx, jobItem); err != nil {
				t.Fatal(err)
			}
			if _, err := e.c.ReportJobByID(ctx, jobID); err != nil {
				t.Fatal(err)
			}
			if _, err := e.c.ReportCampaign(ctx, campaign); err != nil {
				t.Fatal(err)
			}
			reqs := e.stub.Requests()
			if len(reqs) != 6 { // quote, run, two single reports, two pages of 2 and 1 items
				t.Fatalf("%d requests, want 6", len(reqs))
			}
			if problems := sendapitest.CheckTenancy(reqs, userA); len(problems) != 0 {
				t.Fatalf("a request does not carry the tenant: %v", problems)
			}
			for _, r := range reqs {
				if r.Auth != "Bearer "+e.token {
					t.Fatalf("%s %s carries Authorization %q", r.Method, r.Path, r.Auth)
				}
			}
			// control 1: the check names a tenant that is not the one asked for
			if problems := sendapitest.CheckTenancy(reqs, userB); len(problems) != len(reqs) {
				t.Fatalf("the check against another tenant found %d problem(s) over %d requests: %v", len(problems), len(reqs), problems)
			}
			// control 2: a client built for tenant B is caught by the same check against tenant A
			eb := newEnv(t, p, userB, func(w http.ResponseWriter, _ *http.Request, _ int) {
				sendapitest.JSON(w, 200, `{"job_id":"`+jobID+`","state":"queued","attempt_count":0,"reason":null,"error_class":null}`)
			}, modclient.Options{}, sendapi.Options{})
			if _, err := eb.c.ReportJob(ctx, jobItem); err != nil {
				t.Fatal(err)
			}
			if problems := sendapitest.CheckTenancy(eb.stub.Requests(), userA); len(problems) != 1 || !strings.Contains(problems[0], userB) {
				t.Fatalf("the check did not name tenant B's request: %v", problems)
			}
			// control 3: no request at all is an empty subject, not a pass
			if problems := sendapitest.CheckTenancy(nil, userA); len(problems) != 1 || !strings.Contains(problems[0], "empty") {
				t.Fatalf("an empty subject passed: %v", problems)
			}
		})
	}
}

// ---- quote ----------------------------------------------------------------------------------------------------------------------

func TestQuoteDecodesBothModules(t *testing.T) {
	t.Run("module 2: arithmetic only", func(t *testing.T) {
		e := newEnv(t, m2like(), userA, reply(200, `{"recipient_count":3,"cost":6,"currency":"credits","unit_price_credits":2,"tier":"shared"}`), modclient.Options{}, sendapi.Options{})
		q, err := e.c.Quote(ctxT(t), 3)
		if err != nil {
			t.Fatal(err)
		}
		if q.RecipientCount != 3 || q.Cost != 6 || q.UnitPriceCredits != 2 || q.Currency != "credits" || q.Tier != "shared" {
			t.Fatalf("quote = %+v", q)
		}
		if q.Feasible != nil || len(q.InfeasibleReasons) != 0 {
			t.Fatalf("module 2 has no feasibility, the quote carries %v %v", q.Feasible, q.InfeasibleReasons)
		}
	})
	t.Run("module 3: feasibility and the whole body", func(t *testing.T) {
		body := sendapitest.Read(t, fixtures, "quote.ok.json")
		e := newEnv(t, m3like(), userA, serve(200, body), modclient.Options{}, sendapi.Options{})
		q, err := e.c.Quote(ctxT(t), 50)
		if err != nil {
			t.Fatal(err)
		}
		if q.Feasible == nil || !*q.Feasible || q.Cost != 0 || len(q.InfeasibleReasons) != 0 {
			t.Fatalf("quote = %+v", q)
		}
		if !strings.Contains(string(q.Body), `"capacity"`) {
			t.Fatal("the whole body is not carried for the keys only module 3 has")
		}
	})
	t.Run("module 3: an infeasible quote names its reasons", func(t *testing.T) {
		e := newEnv(t, m3like(), userA, reply(200, `{"recipient_count":150000,"cost":0,"currency":"credits","unit_price_credits":0,"tier":"shared","feasible":false,
			"reasons":[{"code":"exceeds_queue_cap","limit":100000,"requested":150000}]}`), modclient.Options{}, sendapi.Options{})
		q, err := e.c.Quote(ctxT(t), 150000)
		if err != nil {
			t.Fatal(err)
		}
		if q.Feasible == nil || *q.Feasible || len(q.InfeasibleReasons) != 1 || q.InfeasibleReasons[0] != "exceeds_queue_cap" {
			t.Fatalf("quote = %+v", q)
		}
	})
}

// A quote that is not an answer is a Contract error naming the key: another count than the one asked, a negative price, a cost that is not count times price (module 2's rule, profile
// data: the same body is fine for module 3, whose cost is inert), and a multiplication that would overflow. The control is the good body of the same shape.
func TestQuoteRefusesWhatIsNotAnAnswer(t *testing.T) {
	good := `{"recipient_count":3,"cost":3,"currency":"credits","unit_price_credits":1,"tier":"shared"}`
	e := newEnv(t, m2like(), userA, reply(200, good), modclient.Options{}, sendapi.Options{})
	if _, err := e.c.Quote(ctxT(t), 3); err != nil {
		t.Fatalf("the control is refused: %v", err)
	}
	cases := []struct{ name, body, key string }{
		{"another count", `{"recipient_count":4,"cost":4,"currency":"credits","unit_price_credits":1,"tier":"shared"}`, "recipient_count"},
		{"negative cost", `{"recipient_count":3,"cost":-3,"currency":"credits","unit_price_credits":-1,"tier":"shared"}`, "cost"},
		{"negative unit", `{"recipient_count":3,"cost":3,"currency":"credits","unit_price_credits":-1,"tier":"shared"}`, "unit_price_credits"},
		{"cost is not count times price", `{"recipient_count":3,"cost":4,"currency":"credits","unit_price_credits":1,"tier":"shared"}`, "cost"},
		{"overflowing product", `{"recipient_count":3,"cost":3,"currency":"credits","unit_price_credits":` + "9223372036854775807" + `,"tier":"shared"}`, "cost"},
		{"missing cost", `{"recipient_count":3,"currency":"credits","unit_price_credits":1,"tier":"shared"}`, "cost"},
		{"cost is text", `{"recipient_count":3,"cost":"3","currency":"credits","unit_price_credits":1,"tier":"shared"}`, "cost"},
		{"cost is fractional", `{"recipient_count":3,"cost":3.5,"currency":"credits","unit_price_credits":1,"tier":"shared"}`, "cost"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, m2like(), userA, reply(200, tc.body), modclient.Options{}, sendapi.Options{})
			q, err := e.c.Quote(ctxT(t), 3)
			ce := wantKind(t, err, modclient.Contract)
			if ce.Key != tc.key {
				t.Fatalf("key %q, want %q: %v", ce.Key, tc.key, err)
			}
			if !isZero(q) {
				t.Fatalf("a quote came back with the error: %+v", q)
			}
		})
	}
	// control: module 3's cost is inert, so a cost that is not count times price is not an error for its profile
	m3 := newEnv(t, m3like(), userA, reply(200, `{"recipient_count":3,"cost":0,"currency":"credits","unit_price_credits":5,"tier":"shared","feasible":true,"reasons":[]}`), modclient.Options{}, sendapi.Options{})
	if _, err := m3.c.Quote(ctxT(t), 3); err != nil {
		t.Fatalf("module 3's inert cost is refused: %v", err)
	}
}

// The count is bounded before anything is sent (module 2 multiplies in 64 bits without a bound). The control: the largest allowed count reaches the network.
func TestQuoteBoundsTheCountItself(t *testing.T) {
	e := newEnv(t, m2like(), userA, reply(200, `{"recipient_count":1000000,"cost":1000000,"currency":"credits","unit_price_credits":1,"tier":"shared"}`), modclient.Options{}, sendapi.Options{})
	for _, n := range []int64{-1, sendapi.MaxRecipientCount + 1, math.MaxInt64} {
		q, err := e.c.Quote(ctxT(t), n)
		wantKind(t, err, modclient.Invalid)
		if !isZero(q) {
			t.Fatalf("a quote came back for %d", n)
		}
	}
	if e.stub.Count() != 0 {
		t.Fatalf("%d request(s) for counts that are refused", e.stub.Count())
	}
	if _, err := e.c.Quote(ctxT(t), sendapi.MaxRecipientCount); err != nil {
		t.Fatalf("the largest allowed count is refused: %v", err)
	}
	if _, err := e.c.Quote(ctxT(t), 0); err == nil || modclient.KindOf(err) != modclient.Contract {
		t.Fatalf("a count of 0 reaches the network (and the stub answers for 1000000): %v", err)
	}
	if e.stub.Count() != 2 {
		t.Fatalf("%d requests, want 2", e.stub.Count())
	}
}

// ---- run ------------------------------------------------------------------------------------------------------------------------

// 202 is a job created, 200 is the replay of an idempotent call with the job's CURRENT state: both are success and the caller can tell which. A 201 is a status the route does not use.
func TestRunCreatedAndReplayAreBothSuccess(t *testing.T) {
	created := newEnv(t, m2like(), userA, reply(202, `{"job_id":"`+jobID+`","state":"queued"}`), modclient.Options{}, sendapi.Options{})
	r, err := created.c.Run(ctxT(t), jobItem)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Created || r.State != "queued" || r.Class != sendapi.ClassPending || r.JobID != jobID {
		t.Fatalf("created = %+v", r)
	}
	replay := newEnv(t, m2like(), userA, reply(200, `{"job_id":"`+jobID+`","state":"sent"}`), modclient.Options{}, sendapi.Options{})
	r, err = replay.c.Run(ctxT(t), jobItem)
	if err != nil {
		t.Fatal(err)
	}
	if r.Created || r.State != "sent" || r.Class != sendapi.ClassTerminal {
		t.Fatalf("a replay of a finished job = %+v (it must not read as queued)", r)
	}
	other := newEnv(t, m2like(), userA, reply(201, `{"job_id":"`+jobID+`","state":"queued"}`), modclient.Options{}, sendapi.Options{})
	r, err = other.c.Run(ctxT(t), jobItem)
	wantKind(t, err, modclient.Contract)
	if !isZero(r) {
		t.Fatalf("a result came back with the error: %+v", r)
	}
}

// The module reports a malformed job_item_id as "required"; the adapter checks every id itself and sends nothing for a bad one. The control: a good id is sent.
func TestIDsAreCheckedBeforeTheNetwork(t *testing.T) {
	e := newEnv(t, m2like(), userA, reply(202, `{"job_id":"`+jobID+`","state":"queued"}`), modclient.Options{}, sendapi.Options{})
	ctx := ctxT(t)
	for _, bad := range []string{"", "not-a-uuid", strings.ToUpper(jobItem), jobItem + "0", "{" + jobItem + "}", strings.ReplaceAll(jobItem, "-", "")} {
		if _, err := e.c.Run(ctx, bad); modclient.KindOf(err) != modclient.Invalid {
			t.Fatalf("Run(%q): %v", bad, err)
		}
		if _, err := e.c.ReportJob(ctx, bad); modclient.KindOf(err) != modclient.Invalid {
			t.Fatalf("ReportJob(%q): %v", bad, err)
		}
		if _, err := e.c.ReportJobByID(ctx, bad); modclient.KindOf(err) != modclient.Invalid {
			t.Fatalf("ReportJobByID(%q): %v", bad, err)
		}
		if _, err := e.c.ReportCampaignPage(ctx, bad, ""); modclient.KindOf(err) != modclient.Invalid {
			t.Fatalf("ReportCampaignPage(%q): %v", bad, err)
		}
		if _, err := e.c.ReportCampaign(ctx, bad); modclient.KindOf(err) != modclient.Invalid {
			t.Fatalf("ReportCampaign(%q): %v", bad, err)
		}
	}
	if _, err := e.c.ReportCampaignPage(ctx, campaign, strings.Repeat("a", 2000)); modclient.KindOf(err) != modclient.Invalid {
		t.Fatalf("a cursor longer than any the module issues: %v", err)
	}
	if e.stub.Count() != 0 {
		t.Fatalf("%d request(s) for bad ids", e.stub.Count())
	}
	if _, err := e.c.Run(ctx, jobItem); err != nil || e.stub.Count() != 1 {
		t.Fatalf("the control (a good id) did not reach the network: %v, %d", err, e.stub.Count())
	}
}

// A 409 on run is the same body for every cause: module 2's profile reads it as back-pressure, module 3's as a refusal of unknown cause, and both still unwrap to the Refused error. The
// controls: a 409 with a code the adapter does not know, a 400 and a 422 are neither (the cause is never guessed from anything but the known code), and the 409 is not retried.
func TestRunConflictMeansWhatTheProfileSays(t *testing.T) {
	conflict := sendapitest.Envelope("conflict")
	for name, tc := range map[string]struct {
		p      sendapi.Profile
		is     error
		isNot  error
		reason string
	}{
		"m2": {m2like(), sendapi.ErrBackpressure, sendapi.ErrConflictUnexplained, "another campaign of this user is active"},
		"m3": {m3like(), sendapi.ErrConflictUnexplained, sendapi.ErrBackpressure, "cause it does not say"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, tc.p, userA, reply(409, conflict), modclient.Options{Retries: 2}, sendapi.Options{})
			r, err := e.c.Run(ctxT(t), jobItem)
			if !isZero(r) {
				t.Fatalf("a result came back with the error: %+v", r)
			}
			ce := wantKind(t, err, modclient.Refused)
			if !errors.Is(err, tc.is) || errors.Is(err, tc.isNot) || !errors.Is(err, modclient.ErrRefused) {
				t.Fatalf("errors.Is: sentinel %v, other %v, refused %v: %v", errors.Is(err, tc.is), errors.Is(err, tc.isNot), errors.Is(err, modclient.ErrRefused), err)
			}
			if !strings.Contains(err.Error(), tc.reason) || ce.Code != "conflict" || ce.Status != 409 {
				t.Fatalf("the error does not say why: %v (code %q, status %d)", err, ce.Code, ce.Status)
			}
			if e.stub.Count() != 1 {
				t.Fatalf("a refusal was retried: %d requests", e.stub.Count())
			}
		})
	}
	for name, body := range map[string]struct {
		status int
		body   string
	}{
		"unknown code on 409":  {409, sendapitest.Envelope("brand_new_code")},
		"validation_failed":    {400, sendapitest.Envelope("validation_failed", [2]string{"user_id", "required"})},
		"unprocessable (m3)":   {422, sendapitest.Envelope("unprocessable", [2]string{"paused", "conflict"})},
		"conflict but a 500":   {500, sendapitest.Envelope("conflict")},
		"unauthenticated":      {401, sendapitest.Envelope("unauthenticated")},
		"conflict on a 404":    {404, sendapitest.Envelope("conflict")},
		"a 409 with no body":   {409, ``},
		"a 409 with a no code": {409, `{"error":{"code":""}}`},
	} {
		t.Run("control "+name, func(t *testing.T) {
			e := newEnv(t, m2like(), userA, reply(body.status, body.body), modclient.Options{}, sendapi.Options{})
			_, err := e.c.Run(ctxT(t), jobItem)
			if err == nil || errors.Is(err, sendapi.ErrBackpressure) || errors.Is(err, sendapi.ErrConflictUnexplained) {
				t.Fatalf("the cause was guessed: %v", err)
			}
		})
	}
}

// 503 paused is Paused (wait, never a retry loop) and 503 unavailable is Unavailable; run is idempotent, so the client retries the second and not the first, and never a refusal.
func TestRunRetriesOnlyWhereARetryCannotHarm(t *testing.T) {
	paused := newEnv(t, m2like(), userA, reply(503, sendapitest.Envelope("paused")), modclient.Options{Retries: 3}, sendapi.Options{})
	_, err := paused.c.Run(ctxT(t), jobItem)
	wantKind(t, err, modclient.Paused)
	if modclient.Retryable(err) || paused.stub.Count() != 1 {
		t.Fatalf("a paused module was asked %d times", paused.stub.Count())
	}
	down := newEnv(t, m2like(), userA, reply(503, sendapitest.Envelope("unavailable")), modclient.Options{Retries: 2}, sendapi.Options{})
	_, err = down.c.Run(ctxT(t), jobItem)
	wantKind(t, err, modclient.Unavailable)
	if down.stub.Count() != 3 {
		t.Fatalf("an idempotent run was asked %d times, want 3 (one attempt and two retries)", down.stub.Count())
	}
	// control: with no retries configured the same failure is asked once, so the three above are the dial and not the adapter
	once := newEnv(t, m2like(), userA, reply(503, sendapitest.Envelope("unavailable")), modclient.Options{}, sendapi.Options{})
	if _, err = once.c.Run(ctxT(t), jobItem); modclient.KindOf(err) != modclient.Unavailable || once.stub.Count() != 1 {
		t.Fatalf("%v after %d requests", err, once.stub.Count())
	}
}

// A 404 says which resource is missing, and the adapter names it without turning it into anything else: not a verdict, not a failure to settle on. The control: a 404 with no details
// names nothing.
func TestNotFoundNamesTheResource(t *testing.T) {
	e := newEnv(t, m2like(), userA, reply(404, sendapitest.Envelope("not_found", [2]string{"job_item_id", "not_found"})), modclient.Options{}, sendapi.Options{})
	_, err := e.c.Run(ctxT(t), jobItem)
	wantKind(t, err, modclient.NotFound)
	if f, ok := sendapi.NotFoundField(err); !ok || f != "job_item_id" {
		t.Fatalf("NotFoundField = %q, %v", f, ok)
	}
	item, err := e.c.ReportJob(ctxT(t), jobItem)
	wantKind(t, err, modclient.NotFound)
	if !isZero(item) {
		t.Fatalf("an item came back for a job the module does not know: %+v", item)
	}
	bare := newEnv(t, m2like(), userA, reply(404, sendapitest.Envelope("not_found")), modclient.Options{}, sendapi.Options{})
	_, err = bare.c.ReportJob(ctxT(t), jobItem)
	if f, ok := sendapi.NotFoundField(err); ok || f != "" {
		t.Fatalf("a 404 with no details names %q", f)
	}
	if f, ok := sendapi.NotFoundField(errors.New("plain")); ok || f != "" {
		t.Fatalf("a plain error names %q", f)
	}
	if f, ok := sendapi.NotFoundField(nil); ok || f != "" {
		t.Fatalf("nil names %q", f)
	}
}

// A state the module advertises to the adapter is the only state the adapter decodes: anything else is a Contract error naming the key, on run, on the single report and on a campaign
// item, and the text of the error never carries the value the module sent.
func TestAStateOutsideTheVocabularyIsRefused(t *testing.T) {
	const alien = "teleported-4f2a"
	e := newEnv(t, m2like(), userA, reply(200, `{"job_id":"`+jobID+`","state":"`+alien+`"}`), modclient.Options{}, sendapi.Options{})
	_, err := e.c.Run(ctxT(t), jobItem)
	if ce := wantKind(t, err, modclient.Contract); ce.Key != "state" || strings.Contains(err.Error(), alien) {
		t.Fatalf("run: key %q, error %v", ce.Key, err)
	}
	e = newEnv(t, m2like(), userA, reply(200, `{"job_id":"`+jobID+`","state":"`+alien+`","attempt_count":0,"reason":null,"error_class":null}`), modclient.Options{}, sendapi.Options{})
	_, err = e.c.ReportJob(ctxT(t), jobItem)
	if ce := wantKind(t, err, modclient.Contract); ce.Key != "state" || strings.Contains(err.Error(), alien) {
		t.Fatalf("report: key %q, error %v", ce.Key, err)
	}
	c := fakeCampaign(m2like(), userA, 3, false)
	c.Items[1]["state"] = alien
	e = newEnv(t, m2like(), userA, c.Handler(), modclient.Options{}, sendapi.Options{})
	rep, err := e.c.ReportCampaign(ctxT(t), campaign)
	if ce := wantKind(t, err, modclient.Contract); ce.Key != "items.results[1].state" || strings.Contains(err.Error(), alien) {
		t.Fatalf("campaign: key %q, error %v", ce.Key, err)
	}
	if !isZero(rep) {
		t.Fatalf("a report came back with the error: %+v", rep)
	}
	// control: module 3 does not have `cancelled`, so the same state that module 2 accepts is refused by its profile
	e3 := newEnv(t, m3like(), userA, reply(200, `{"job_id":"`+jobID+`","state":"cancelled"}`), modclient.Options{}, sendapi.Options{})
	if _, err := e3.c.Run(ctxT(t), jobItem); modclient.KindOf(err) != modclient.Contract {
		t.Fatalf("module 3's profile accepts cancelled: %v", err)
	}
	e2 := newEnv(t, m2like(), userA, reply(200, `{"job_id":"`+jobID+`","state":"cancelled"}`), modclient.Options{}, sendapi.Options{})
	if r, err := e2.c.Run(ctxT(t), jobItem); err != nil || r.Class != sendapi.ClassTerminal {
		t.Fatalf("module 2's profile refuses cancelled: %v %+v", err, r)
	}
}

// ---- the single report ---------------------------------------------------------------------------------------------------

// Module 2 sends reason and error_class as keys (null when there is nothing to say) and omits provider_message_id and completed_at until set; module 3 omits everything optional until
// set and carries account detail. One decoder reads both; Item.JobItemID is the key the caller asked with because module 2's single report does not carry it back.
func TestReportJobDecodesTheTwoConventions(t *testing.T) {
	t.Run("module 2 sent", func(t *testing.T) {
		e := newEnv(t, m2like(), userA, reply(200, `{"job_id":"`+jobID+`","state":"sent","attempt_count":0,"reason":null,"error_class":null,"provider_message_id":"<j@mail.example.test>","completed_at":"2026-11-17T10:00:00.123456Z"}`), modclient.Options{}, sendapi.Options{})
		it, err := e.c.ReportJob(ctxT(t), jobItem)
		if err != nil {
			t.Fatal(err)
		}
		if it.JobItemID != jobItem || it.JobID != jobID || it.State != "sent" || it.Class != sendapi.ClassTerminal || it.Reason != nil || it.ErrorClass != nil {
			t.Fatalf("item = %+v", it)
		}
		if it.ProviderMessageID == nil || *it.ProviderMessageID != "<j@mail.example.test>" || it.CompletedAt == nil || it.CompletedAt.Nanosecond() != 123456000 {
			t.Fatalf("provider id or completion time: %+v", it)
		}
		if it.SMTPResponse != nil || it.AccountID != nil {
			t.Fatalf("module 2 has no account detail: %+v", it)
		}
		q := e.stub.Requests()[0].Query
		if q.Get("job_item_id") != jobItem || q.Get("job_id") != "" || q.Get("campaign_id") != "" {
			t.Fatalf("selectors of a report by job item: %v", q)
		}
	})
	t.Run("module 2 queued with a reason", func(t *testing.T) {
		e := newEnv(t, m2like(), userA, reply(200, `{"job_id":"`+jobID+`","state":"queued","attempt_count":1,"reason":"waiting_for_capacity","error_class":"transport_4xx"}`), modclient.Options{}, sendapi.Options{})
		it, err := e.c.ReportJob(ctxT(t), jobItem)
		if err != nil {
			t.Fatal(err)
		}
		if it.Class != sendapi.ClassPending || it.Reason == nil || *it.Reason != "waiting_for_capacity" || it.ErrorClass == nil || *it.ErrorClass != "transport_4xx" || it.CompletedAt != nil {
			t.Fatalf("item = %+v", it)
		}
	})
	t.Run("module 3 omits until set", func(t *testing.T) {
		e := newEnv(t, m3like(), userA, serve(200, sendapitest.Read(t, fixtures, "report.job.queued.json")), modclient.Options{}, sendapi.Options{})
		it, err := e.c.ReportJobByID(ctxT(t), jobID)
		if err != nil {
			t.Fatal(err)
		}
		if it.JobItemID != "" || it.State != "queued" || it.SMTPResponse == nil || *it.SMTPResponse == "" || it.ErrorClass == nil || it.AccountID != nil || it.Reason != nil || it.FailedAt != nil {
			t.Fatalf("item = %+v", it)
		}
		if q := e.stub.Requests()[0].Query; q.Get("job_id") != jobID || q.Get("job_item_id") != "" {
			t.Fatalf("selectors of a report by job id: %v", q)
		}
		sent := newEnv(t, m3like(), userA, serve(200, sendapitest.Read(t, fixtures, "report.job.sent.json")), modclient.Options{}, sendapi.Options{})
		it, err = sent.c.ReportJob(ctxT(t), jobItem)
		if err != nil {
			t.Fatal(err)
		}
		if it.AccountID == nil || it.AccountUsername == nil || it.ErrorClass != nil || it.SMTPResponse != nil || it.Class != sendapi.ClassTerminal {
			t.Fatalf("item = %+v", it)
		}
	})
}

// "Always present" is profile data: a single report of module 2 without `reason` or `error_class` is a Contract error naming the key (a missing key must not read as null), while the same
// omission is the normal form for module 3. The campaign item is the other convention: every key present, null when unset, on both.
func TestSingleReportKeysAreRequiredOrOptionalByProfile(t *testing.T) {
	base := `{"job_id":"` + jobID + `","state":"sent","attempt_count":0,%s"provider_message_id":"<j@mail.example.test>","completed_at":"2026-11-17T10:00:00Z"}`
	for _, tc := range []struct{ name, missing, key string }{
		{"no reason", `"error_class":null,`, "reason"},
		{"no error_class", `"reason":null,`, "error_class"},
	} {
		t.Run("module 2 "+tc.name, func(t *testing.T) {
			e := newEnv(t, m2like(), userA, reply(200, strings.Replace(base, "%s", tc.missing, 1)), modclient.Options{}, sendapi.Options{})
			_, err := e.c.ReportJob(ctxT(t), jobItem)
			if ce := wantKind(t, err, modclient.Contract); ce.Key != tc.key {
				t.Fatalf("key %q, want %q", ce.Key, tc.key)
			}
		})
	}
	e := newEnv(t, m3like(), userA, reply(200, strings.Replace(base, "%s", "", 1)), modclient.Options{}, sendapi.Options{})
	if _, err := e.c.ReportJob(ctxT(t), jobItem); err != nil {
		t.Fatalf("module 3 omits the optional keys and is refused: %v", err)
	}
	c := fakeCampaign(m3like(), userA, 2, false)
	delete(c.Items[0], "smtp_response") // a campaign item of module 3 must carry every key, null when unset
	e = newEnv(t, m3like(), userA, c.Handler(), modclient.Options{}, sendapi.Options{})
	_, err := e.c.ReportCampaign(ctxT(t), campaign)
	if ce := wantKind(t, err, modclient.Contract); ce.Key != "items.results[0].smtp_response" {
		t.Fatalf("key %q", ce.Key)
	}
}

// completed_at has two formats and both parse; an unreadable one is a Contract error naming the key (never a zero time).
func TestTimestampsAreReadInBothFormats(t *testing.T) {
	for _, ts := range []string{"2026-11-17T10:00:00Z", "2026-11-17T10:00:00.5Z", "2026-11-17T10:00:00.123456Z", "2026-11-17T12:00:00+02:00"} {
		e := newEnv(t, m2like(), userA, reply(200, `{"job_id":"`+jobID+`","state":"sent","attempt_count":0,"reason":null,"error_class":null,"completed_at":"`+ts+`"}`), modclient.Options{}, sendapi.Options{})
		it, err := e.c.ReportJob(ctxT(t), jobItem)
		if err != nil || it.CompletedAt == nil || it.CompletedAt.Year() != 2026 {
			t.Fatalf("%q: %v %+v", ts, err, it)
		}
	}
	for _, ts := range []string{"yesterday", "2026-11-17", "2026-11-17 10:00:00", ""} {
		e := newEnv(t, m2like(), userA, reply(200, `{"job_id":"`+jobID+`","state":"sent","attempt_count":0,"reason":null,"error_class":null,"completed_at":"`+ts+`"}`), modclient.Options{}, sendapi.Options{})
		it, err := e.c.ReportJob(ctxT(t), jobItem)
		if ce := wantKind(t, err, modclient.Contract); ce.Key != "completed_at" || !isZero(it) {
			t.Fatalf("%q: key %q item %+v", ts, ce.Key, it)
		}
	}
}

// ---- no error path returns a value ----------------------------------------------------------------------------------

// Every method, against every way a module can fail, returns an error of a known kind AND the zero value. The control is the same method against a healthy module: its value is NOT zero,
// so the zero-value assertion above it can fail.
func TestNoErrorPathReturnsAValue(t *testing.T) {
	ops := []struct {
		name string
		call func(c *sendapi.Client) (any, error)
	}{
		{"Quote", func(c *sendapi.Client) (any, error) { return c.Quote(ctxT(t), 50) }},
		{"Run", func(c *sendapi.Client) (any, error) { return c.Run(ctxT(t), jobItem) }},
		{"ReportJob", func(c *sendapi.Client) (any, error) { return c.ReportJob(ctxT(t), jobItem) }},
		{"ReportJobByID", func(c *sendapi.Client) (any, error) { return c.ReportJobByID(ctxT(t), jobID) }},
		{"ReportCampaignPage", func(c *sendapi.Client) (any, error) { return c.ReportCampaignPage(ctxT(t), campaign, "") }},
		{"ReportCampaign", func(c *sendapi.Client) (any, error) { return c.ReportCampaign(ctxT(t), campaign) }},
		{"Walk", func(c *sendapi.Client) (any, error) {
			return c.Walk(ctxT(t), campaign, func(sendapi.CampaignPage) error { return nil })
		}},
	}
	healthy := func(w http.ResponseWriter, r *http.Request, _ int) {
		switch {
		case r.URL.Path == "/v1/quote":
			sendapitest.JSON(w, 200, string(sendapitest.Read(t, fixtures, "quote.ok.json")))
		case r.URL.Path == "/v1/run":
			sendapitest.JSON(w, 202, string(sendapitest.Read(t, fixtures, "run.created.json")))
		case r.URL.Query().Get("campaign_id") != "":
			sendapitest.JSON(w, 200, string(sendapitest.Read(t, fixtures, "report.campaign.last_page.json")))
		default:
			sendapitest.JSON(w, 200, string(sendapitest.Read(t, fixtures, "report.job.sent.json")))
		}
	}
	failures := []struct {
		name string
		h    handler
		kind modclient.Kind
	}{
		{"500", reply(500, sendapitest.Envelope("internal_error")), modclient.Internal},
		{"503 paused", reply(503, sendapitest.Envelope("paused")), modclient.Paused},
		{"503 unavailable", reply(503, sendapitest.Envelope("unavailable")), modclient.Unavailable},
		{"404", reply(404, sendapitest.Envelope("not_found", [2]string{"job_id", "not_found"})), modclient.NotFound},
		{"409", reply(409, sendapitest.Envelope("conflict")), modclient.Refused},
		{"401", reply(401, sendapitest.Envelope("unauthenticated")), modclient.Refused},
		{"429", reply(429, sendapitest.Envelope("rate_limited")), modclient.RateLimited},
		{"truncated body", func(w http.ResponseWriter, _ *http.Request, _ int) {
			sendapitest.JSON(w, 200, `{"job_id":"x","state":"que`)
		}, modclient.Contract},
		{"not json", func(w http.ResponseWriter, _ *http.Request, _ int) { sendapitest.JSON(w, 200, `<html>`) }, modclient.Contract},
		{"an array", reply(200, `[]`), modclient.Contract},
		{"an empty object", reply(200, `{}`), modclient.Contract},
		{"wrong content type", func(w http.ResponseWriter, _ *http.Request, _ int) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`{"job_id":"x"}`))
		}, modclient.Contract},
		{"a redirect", func(w http.ResponseWriter, _ *http.Request, _ int) {
			w.Header().Set("Location", "http://redirect.invalid/")
			w.WriteHeader(302)
		}, modclient.Contract},
	}
	for _, op := range ops {
		for _, f := range failures {
			t.Run(op.name+" "+f.name, func(t *testing.T) {
				e := newEnv(t, m3like(), userA, f.h, modclient.Options{}, sendapi.Options{})
				v, err := op.call(e.c)
				wantKind(t, err, f.kind)
				if !isZero(v) {
					t.Fatalf("a value came back with the error: %+v", v)
				}
			})
		}
		t.Run(op.name+" module down", func(t *testing.T) {
			e := newEnv(t, m3like(), userA, healthy, modclient.Options{}, sendapi.Options{})
			e.stub.Close()
			v, err := op.call(e.c)
			wantKind(t, err, modclient.Unavailable)
			if !isZero(v) {
				t.Fatalf("a value came back with the error: %+v", v)
			}
		})
		t.Run(op.name+" control: healthy", func(t *testing.T) {
			e := newEnv(t, m3like(), userA, healthy, modclient.Options{}, sendapi.Options{})
			v, err := op.call(e.c)
			if err != nil {
				t.Fatalf("the healthy module is refused: %v", err)
			}
			if isZero(v) {
				t.Fatal("the healthy module's value is zero, so the zero-value assertion could not fail")
			}
		})
	}
}

// The error of a refused response never carries a value the module sent: not the state, not an id, not the bearer token the module echoes back.
func TestErrorsCarryNoModuleText(t *testing.T) {
	token := sendapitest.Token(t)
	const marker = "zzmarker-91c7"
	stub := sendapitest.NewStub(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		sendapitest.JSON(w, 200, `{"job_id":"`+marker+`","state":"`+marker+`","attempt_count":0,"reason":"`+marker+`","error_class":null,"echo":"`+token+`"}`)
	})
	mc := sendapitest.ModClient(t, modclient.M2, stub.URL(), token, modclient.Options{})
	c, err := sendapi.New(mc, m2like(), userA, sendapi.Options{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.ReportJob(ctxT(t), jobItem)
	wantKind(t, err, modclient.Contract)
	if n := contains(err.Error(), marker, token); n != "" {
		t.Fatalf("the error carries %q: %v", n, err)
	}
	// control: the same bytes with a state the adapter knows decode fine and the module's text IS carried in the item, so its absence from the error above is a property of the error
	ok := sendapitest.NewStub(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		sendapitest.JSON(w, 200, `{"job_id":"`+jobID+`","state":"sent","attempt_count":0,"reason":"`+marker+`","error_class":null}`)
	})
	c, err = sendapi.New(sendapitest.ModClient(t, modclient.M2, ok.URL(), token, modclient.Options{}), m2like(), userA, sendapi.Options{})
	if err != nil {
		t.Fatal(err)
	}
	it, err := c.ReportJob(ctxT(t), jobItem)
	if err != nil || it.Reason == nil || *it.Reason != marker {
		t.Fatalf("the control did not decode: %v %+v", err, it)
	}
}

// The caller's context ends the call: Canceled, nothing returned, and a walk stops at once.
func TestACanceledContextEndsTheCall(t *testing.T) {
	e := newEnv(t, m2like(), userA, fakeCampaign(m2like(), userA, 5, false).Handler(), modclient.Options{}, sendapi.Options{PageSize: 1})
	ctx, cancel := contextWithCancel()
	cancel()
	if _, err := e.c.ReportCampaign(ctx, campaign); modclient.KindOf(err) != modclient.Canceled {
		t.Fatalf("a canceled context: %v", err)
	}
	if e.stub.Count() != 0 {
		t.Fatalf("%d request(s) after cancellation", e.stub.Count())
	}
	if _, err := e.c.ReportCampaign(ctxT(t), campaign); err != nil || e.stub.Count() != 5 {
		t.Fatalf("control: %v after %d requests", err, e.stub.Count())
	}
}
