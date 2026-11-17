package m3_test

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/adapters/m3"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi/sendapitest"
	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/m3admin"
	"github.com/coldmail-platform/platform/internal/modclient"
	"github.com/coldmail-platform/platform/internal/tenant"
)

// ---- the profile ------------------------------------------------------------------------------------------------------

// Module 3's profile is what the research documents and the module's code says (checked at the pinned commit: send_byo.job_state in enum order, send_byo.error_class, the nine codes of
// describeErrorCodes in status order, no `reason` key, a single report that omits its optional keys until set, account detail, a feasibility quote with an inert price, and a 409 on run
// with four causes). The control changes one field and the comparison fails.
func TestProfileIsTheDocumentedOne(t *testing.T) {
	want := sendapi.Profile{
		Module: modclient.M3, DescribeModule: "send_byo",
		JobStates:        []string{"queued", "assigned", "sending", "sent", "failed", "bounced"},
		ErrorClasses:     []string{"bounce_permanent", "transport_4xx", "auth_failure", "throttle", "timeout", "unknown"},
		ErrorCodes:       []string{"validation_failed", "unauthenticated", "not_found", "method_not_allowed", "conflict", "unprocessable", "rate_limited", "internal_error", "paused"},
		AccountDetail:    true,
		QuoteFeasibility: true,
		RunConflict:      sendapi.ConflictUnexplained,
	}
	if got := m3.Profile(); !reflect.DeepEqual(got, want) {
		t.Fatalf("the profile is not the documented one:\n got %+v\nwant %+v", got, want)
	}
	changed := want
	changed.AlwaysPresent = true
	if reflect.DeepEqual(m3.Profile(), changed) {
		t.Fatal("the control: a different profile compares equal")
	}
	p := m3.Profile()
	p.JobStates[0] = "tampered"
	if m3.Profile().JobStates[0] != "queued" {
		t.Fatal("Profile shares its slices between calls")
	}
	if slicesHas(m3.Profile().JobStates, "cancelled") || len(m3.Profile().ItemReasons) != 0 {
		t.Fatal("module 3 has no cancelled state and no reason key")
	}
	s := newStack(t, nil, modclient.Options{})
	if _, err := s.a.For(userA); err != nil {
		t.Fatal(err)
	}
}

func slicesHas(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// ---- the construction -------------------------------------------------------------------------------------------------

// authProblems says what is wrong with the credentials a stub received: every request must carry exactly the wanted bearer, and none the unwanted one. It compares digests so that no token
// is ever printed.
func authProblems(reqs []sendapitest.Seen, want, unwanted string) []string {
	if len(reqs) == 0 {
		return []string{"no request was made: the subject of the check is empty"}
	}
	var problems []string
	for _, r := range reqs {
		got := strings.TrimPrefix(r.Auth, "Bearer ")
		switch {
		case !strings.HasPrefix(r.Auth, "Bearer "):
			problems = append(problems, "request "+r.Path+" carries no bearer credential")
		case got == unwanted:
			problems = append(problems, "request "+r.Path+" carries the unwanted credential "+digest(unwanted))
		case got != want:
			problems = append(problems, "request "+r.Path+" carries credential "+digest(got)+", want "+digest(want))
		}
	}
	return problems
}

// FromConfig builds the adapter from PLATFORM_M3_URL and PLATFORM_M3_SERVICE_TOKEN and the shared module-call dials. Either missing is ErrNotConfigured and a modclient.Invalid error that
// names the variable that is missing (and not the other one); nothing is sent. With both, the adapter calls the configured URL with the SERVICE token, never the admin key, and retries an
// idempotent call as many times as the dial says.
func TestFromConfig(t *testing.T) {
	stub := sendapitest.NewStub(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		sendapitest.JSON(w, 503, sendapitest.Envelope("unavailable"))
	})
	service, admin := sendapitest.Token(t), sendapitest.Token(t)
	good := config.Config{M3URL: stub.URL(), M3ServiceToken: config.Secret(service), M3AdminToken: config.Secret(admin), ModuleTimeout: 2 * time.Second, ModuleRetries: 2}
	a, err := m3.FromConfig(good)
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
	if len(reqs) != 3 || reqs[0].Path != "/v1/run" {
		t.Fatalf("%d request(s): the dials did not arrive", len(reqs))
	}
	// the SERVICE token is what is sent, and never the admin key (digests, so no token is printed)
	if problems := authProblems(reqs, service, admin); len(problems) != 0 {
		t.Fatalf("%v", problems)
	}
	// paired control: the same check names an adapter built with the admin key as its credential
	wrong := newStack(t, reply(503, sendapitest.Envelope("unavailable")), modclient.Options{})
	wrongMC := sendapitest.ModClient(t, modclient.M3, wrong.stub.URL(), admin, modclient.Options{})
	wa, err := m3.NewAdapter(wrongMC)
	if err != nil {
		t.Fatal(err)
	}
	wc, _ := wa.For(userA)
	_, _ = wc.Run(ctxT(t), jobItem)
	if p := authProblems(wrong.stub.Requests(), service, admin); len(p) != 1 || !strings.Contains(p[0], "unwanted credential") {
		t.Fatalf("an adapter carrying the admin key was not caught: %v", p)
	}
	// the dials are consumed: no retries means one request
	stub2 := sendapitest.NewStub(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		sendapitest.JSON(w, 503, sendapitest.Envelope("unavailable"))
	})
	none := good
	none.M3URL, none.ModuleRetries = stub2.URL(), 0
	a2, err := m3.FromConfig(none)
	if err != nil {
		t.Fatal(err)
	}
	c2, _ := a2.For(userA)
	_, _ = c2.Run(ctxT(t), jobItem)
	if stub2.Count() != 1 {
		t.Fatalf("with no retries %d requests were made", stub2.Count())
	}

	// not configured: each refusal names the variable that is missing and sends nothing
	for name, tc := range map[string]struct {
		cfg  config.Config
		must []string
		not  []string
	}{
		"nothing":  {config.Config{}, []string{config.VarM3URL, config.VarM3ServiceToken}, nil},
		"no URL":   {config.Config{M3ServiceToken: config.Secret(service)}, []string{config.VarM3URL}, []string{config.VarM3ServiceToken}},
		"no token": {config.Config{M3URL: stub.URL(), M3AdminToken: config.Secret(admin)}, []string{config.VarM3ServiceToken}, []string{config.VarM3URL}},
	} {
		t.Run("refuses "+name, func(t *testing.T) {
			before := stub.Count()
			a, err := m3.FromConfig(tc.cfg)
			if a != nil || err == nil {
				t.Fatalf("an adapter came back: %v", err)
			}
			wantKind(t, err, modclient.Invalid)
			if !errors.Is(err, m3.ErrNotConfigured) {
				t.Fatalf("not ErrNotConfigured: %v", err)
			}
			for _, m := range tc.must {
				if !strings.Contains(err.Error(), m) {
					t.Fatalf("the refusal does not name %s: %v", m, err)
				}
			}
			for _, n := range tc.not {
				if strings.Contains(err.Error(), n) {
					t.Fatalf("the refusal names %s, which is set: %v", n, err)
				}
			}
			if stub.Count() != before {
				t.Fatal("a refused configuration sent a request")
			}
		})
	}
	for name, cfg := range map[string]config.Config{
		"bad URL":   {M3URL: "ftp://x.example", M3ServiceToken: config.Secret(service)},
		"bad token": {M3URL: stub.URL(), M3ServiceToken: config.Secret("has space")},
	} {
		if a, err := m3.FromConfig(cfg); a != nil || modclient.KindOf(err) != modclient.Invalid {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// the service token must not be the admin key: the two domains never share a credential
	same := good
	same.M3AdminToken = config.Secret(service)
	a3, err := m3.FromConfig(same)
	if a3 != nil || !errors.Is(err, m3.ErrSharedCredential) || modclient.KindOf(err) != modclient.Invalid {
		t.Fatalf("a shared credential: %v", err)
	}
	if strings.Contains(err.Error(), service) {
		t.Fatal("the refusal carries the token")
	}
	// what Load yields for the variables is what FromConfig consumes
	env := map[string]string{
		config.VarDatabaseURL: "postgres://platform_app@db.example:5432/postgres", config.VarM3URL: stub.URL(), config.VarM3AdminToken: admin, config.VarM3ServiceToken: service,
		config.VarModuleRetries: "1", config.VarModuleRetryBackoffMS: "0",
	}
	cfg, err := config.Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	a4, err := m3.FromConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	before := stub.Count()
	c4, _ := a4.For(userA)
	_, _ = c4.Run(ctxT(t), jobItem)
	if got := stub.Count() - before; got != 2 {
		t.Fatalf("PLATFORM_MODULE_RETRIES=1 gave %d requests, want 2", got)
	}
	if p := authProblems(stub.Requests()[before:], service, admin); len(p) != 0 {
		t.Fatalf("%v", p)
	}
	// unset in the environment is "not configured", not a silent no-op
	delete(env, config.VarM3ServiceToken)
	cfg, err = config.Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if a5, err := m3.FromConfig(cfg); a5 != nil || !errors.Is(err, m3.ErrNotConfigured) || !strings.Contains(err.Error(), config.VarM3ServiceToken) {
		t.Fatalf("an unset service token: %v", err)
	}
	// and the adapter's errors never carry a credential
	if _, err := m3.FromConfig(config.Config{M3URL: "http://user:" + service + "@x.example", M3ServiceToken: config.Secret(service)}); err == nil || strings.Contains(err.Error(), service) {
		t.Fatalf("a credential in the URL: %v", err)
	}
}

func TestNewAdapterRefusesAnotherModulesClient(t *testing.T) {
	stub := sendapitest.NewStub(t, func(w http.ResponseWriter, _ *http.Request, _ int) { sendapitest.JSON(w, 500, "{}") })
	for name, mc := range map[string]*modclient.Client{
		"module 2": sendapitest.ModClient(t, modclient.M2, stub.URL(), sendapitest.Token(t), modclient.Options{}),
		"module 1": sendapitest.ModClient(t, modclient.M1, stub.URL(), sendapitest.Token(t), modclient.Options{}),
		"nil":      nil,
	} {
		a, err := m3.NewAdapter(mc)
		if a != nil {
			t.Fatalf("%s: an adapter came back", name)
		}
		wantKind(t, err, modclient.Invalid)
	}
	if _, err := m3.NewAdapter(sendapitest.ModClient(t, modclient.M3, stub.URL(), sendapitest.Token(t), modclient.Options{})); err != nil {
		t.Fatalf("control: %v", err)
	}
	if stub.Count() != 0 {
		t.Fatal("a constructor sent a request")
	}
}

// ---- tenancy ------------------------------------------------------------------------------------------------------------

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

// route is where a request must carry the tenant: the body of quote, run, create and bulk add, the query of everything else.
type placement int

const (
	inBody placement = iota + 1
	inQuery
)

func placementOf(method, path string) placement {
	switch {
	case path == "/v1/quote" || path == "/v1/run" || (method == http.MethodPost && (path == "/v1/send-byo/accounts" || path == "/v1/send-byo/accounts/bulk")):
		return inBody
	}
	return inQuery
}

// tenancyProblems says which request does not carry the tenant where the module reads it. A POST that the module reads the tenant from the body of must carry it THERE (a query value would
// be ignored and the module would answer 400), and every other request carries it in the query.
func tenancyProblems(reqs []sendapitest.Seen, user string) []string {
	if len(reqs) == 0 {
		return []string{"no request was made: the subject of the check is empty"}
	}
	var problems []string
	for _, r := range reqs {
		var got string
		if placementOf(r.Method, r.Path) == inBody {
			got = bodyUser(r.Body)
		} else {
			got = r.Query.Get("user_id")
		}
		if got != user {
			problems = append(problems, r.Method+" "+r.Path+" carries user_id "+got+", want "+user)
		}
	}
	return problems
}

func bodyUser(b []byte) string {
	s := string(b)
	i := strings.Index(s, `"user_id":"`)
	if i < 0 {
		return ""
	}
	s = s[i+len(`"user_id":"`):]
	if j := strings.IndexByte(s, '"'); j >= 0 {
		return s[:j]
	}
	return ""
}

// moduleStub answers every route of the adapter's surface from its recorded body.
func moduleStub(t testing.TB) handler {
	good := func(name string) string { return string(sendapitest.Read(t, fixtures, name)) }
	return func(w http.ResponseWriter, r *http.Request, _ []byte) {
		p := r.URL.Path
		switch {
		case p == "/v1/quote":
			sendapitest.JSON(w, 200, good("quote.feasible.json"))
		case p == "/v1/run":
			sendapitest.JSON(w, 202, good("run.created.json"))
		case p == "/v1/report" && r.URL.Query().Get("campaign_id") != "":
			sendapitest.JSON(w, 200, good("report.campaign.last_page.json"))
		case p == "/v1/report":
			sendapitest.JSON(w, 200, good("report.job.sent.json"))
		case p == "/v1/send-byo/accounts" && r.Method == http.MethodPost:
			sendapitest.JSON(w, 201, good("accounts.create.created.json"))
		case p == "/v1/send-byo/accounts":
			sendapitest.JSON(w, 200, good("accounts.list.last_page.json"))
		case p == "/v1/send-byo/accounts/bulk":
			sendapitest.JSON(w, 200, good("accounts.bulk.ok.json"))
		case strings.HasSuffix(p, "/test"):
			sendapitest.JSON(w, 200, good("accounts.test.ok.json"))
		case strings.HasSuffix(p, "/events"):
			sendapitest.JSON(w, 200, good("accounts.events.page.json"))
		default:
			sendapitest.JSON(w, 200, good("accounts.get.ok.json"))
		}
	}
}

// Every call of module 3's client carries the tenant it was built for, where the module reads it (body for quote, feasibility, run, create and bulk add; query for the reports, the lists, get,
// the test button and events), and the service token. The same check run on a client built for another tenant names it.
func TestEveryCallCarriesItsTenant(t *testing.T) {
	s := newStack(t, moduleStub(t), modclient.Options{})
	c := s.client(t, userA)
	ctx := ctxT(t)
	steps := map[string]func() error{
		"quote":       func() error { _, err := c.Quote(ctx, 5000); return err },
		"feasibility": func() error { _, err := c.Feasibility(ctx, 5000); return err },
		"run":         func() error { _, err := c.Run(ctx, jobItem); return err },
		"report job":  func() error { _, err := c.ReportJob(ctx, jobItem); return err },
		"report camp": func() error { _, err := c.ReportCampaign(ctx, campaign); return err },
		"list":        func() error { _, err := c.ListAccounts(ctx, ""); return err },
		"all":         func() error { _, err := c.AllAccounts(ctx); return err },
		"get":         func() error { _, err := c.GetAccount(ctx, acct1); return err },
		"create":      func() error { _, err := c.CreateAccount(ctx, newAcct(t, 1)); return err },
		"test":        func() error { _, err := c.TestAccount(ctx, acct1, false); return err },
		"bulk":        func() error { _, err := c.BulkAddAccounts(ctx, batch(t, 5)); return err },
		"events":      func() error { _, err := c.AccountEvents(ctx, acct1, ""); return err },
	}
	for name, step := range steps {
		before := s.stub.Count()
		if err := step(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		reqs := s.stub.Requests()[before:]
		if len(reqs) == 0 {
			t.Fatalf("%s: no request", name)
		}
		if problems := tenancyProblems(reqs, userA); len(problems) != 0 {
			t.Fatalf("%s: %v", name, problems)
		}
		if problems := authProblems(reqs, s.token, ""); len(problems) != 0 {
			t.Fatalf("%s: %v", name, problems)
		}
	}
	// control: the same check names a client for another tenant, for a body route and for a query route
	other := s.client(t, userB)
	before := s.stub.Count()
	if _, err := other.CreateAccount(ctx, newAcct(t, 1)); err == nil {
		// the stub answers a body for tenant A, which the client refuses: the REQUEST is what the control inspects
		t.Fatal("an account of another tenant was accepted")
	}
	_, _ = other.ListAccounts(ctx, "")
	got := tenancyProblems(s.stub.Requests()[before:], userA)
	if len(got) != 2 || !strings.Contains(got[0], userB) || !strings.Contains(got[1], userB) {
		t.Fatalf("the check did not name tenant B's requests: %v", got)
	}
}

// ---- no cancel -----------------------------------------------------------------------------------------------------------

// cancelMethods lists the methods of a type whose name says cancel: module 3 has no such route, and a method that pretended would invent a behaviour the module does not have.
func cancelMethods(v any) []string {
	var out []string
	rt := reflect.TypeOf(v)
	for i := 0; i < rt.NumMethod(); i++ {
		if n := rt.Method(i).Name; strings.Contains(strings.ToLower(n), "cancel") {
			out = append(out, n)
		}
	}
	return out
}

type pretender struct{}

func (pretender) Cancel() {}

// Module 3 has no cancel route and no cancelled state: neither the tenant's Client (the shared body's methods included) nor the Adapter has a method that says cancel. The control runs the
// same check on a type that has one.
func TestThereIsNoCancel(t *testing.T) {
	for name, v := range map[string]any{"*Client": (*m3.Client)(nil), "*Adapter": (*m3.Adapter)(nil), "*sendapi.Client": (*sendapi.Client)(nil)} {
		if got := cancelMethods(v); len(got) != 0 {
			t.Fatalf("%s has %v: module 3 has no cancel", name, got)
		}
		if reflect.TypeOf(v).NumMethod() < 5 {
			t.Fatalf("%s has %d methods: the instrument matched nothing", name, reflect.TypeOf(v).NumMethod())
		}
	}
	if got := cancelMethods(pretender{}); len(got) != 1 {
		t.Fatalf("the control did not catch a Cancel method: %v", got)
	}
	if slicesHas(m3.Profile().JobStates, "cancelled") {
		t.Fatal("module 3's profile lists a cancelled state")
	}
}

// ---- describe -------------------------------------------------------------------------------------------------------------

// Module 3's recorded describe advertises exactly what the adapter handles (no break, nothing additive), and CheckVocabularies names each way that stops being true: a state gone, a state
// the adapter does not handle, an error class gone, an error code gone, another module's describe. An additive error class is reported, not raised; a module that is down is a typed error and
// nothing else.
func TestCheckVocabulariesAgainstModule3sDescribe(t *testing.T) {
	good := sendapitest.Read(t, fixtures, "describe.ok.json")
	run := func(t *testing.T, body []byte) (sendapi.VocabReport, error) {
		s := newStack(t, reply(200, string(body)), modclient.Options{})
		return s.a.CheckVocabularies(ctxT(t))
	}
	rep, err := run(t, good)
	if err != nil || rep.Breaking() || rep.Additive() || len(rep.Diffs) != 2 {
		t.Fatalf("the recorded describe: %v %+v", err, rep)
	}
	for _, d := range rep.Diffs {
		if !d.Exact() {
			t.Fatalf("%s is not exact: %+v", d.Name, d)
		}
	}
	vs, err := newStack(t, reply(200, string(good)), modclient.Options{}).a.Vocabularies(ctxT(t))
	if err != nil || !reflect.DeepEqual(vs["job_state"], m3.Profile().JobStates) {
		t.Fatalf("%v %v", err, vs)
	}
	mut := func(path string, op sendapitest.Op, v any) []byte {
		b, err := sendapitest.Mutate(good, path, op, v)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	for name, tc := range map[string]struct {
		body []byte
		key  string
		msg  string
	}{
		"bounced is gone":        {mut("vocabularies.job_state.values[5]", sendapitest.Delete, nil), "vocabularies.job_state", "bounced"},
		"a new state":            {mut("vocabularies.job_state.values[5]", sendapitest.Set, "cancelled"), "vocabularies.job_state", "bounced"},
		"an error class gone":    {mut("vocabularies.error_class.values[2]", sendapitest.Delete, nil), "vocabularies.error_class", "auth_failure"},
		"an error code gone":     {mut("error_codes[5]", sendapitest.Delete, nil), "error_codes", "unprocessable"},
		"module 2's describe":    {mut("module", sendapitest.Set, "send_managed"), "module", "wired"},
		"another api version":    {mut("api_version", sendapitest.Set, "v2"), "api_version", "version"},
		"the vocabulary is gone": {mut("vocabularies.error_class", sendapitest.Delete, nil), "vocabularies.error_class", "no longer advertises"},
	} {
		t.Run(name, func(t *testing.T) {
			rep, err := run(t, tc.body)
			ce := wantKind(t, err, modclient.Contract)
			if ce.Key != tc.key || !strings.Contains(ce.Message, tc.msg) {
				t.Fatalf("key %q message %q, want %q containing %q", ce.Key, ce.Message, tc.key, tc.msg)
			}
			if rep.Breaking() || len(rep.Diffs) != 0 {
				t.Fatalf("a report came back with the error: %+v", rep)
			}
		})
	}
	// an additive error class is not an error
	extra, err := sendapitest.Mutate(good, "vocabularies.error_class.values", sendapitest.Set, []string{"bounce_permanent", "transport_4xx", "auth_failure", "throttle", "timeout", "unknown", "held"})
	if err != nil {
		t.Fatal(err)
	}
	rep, err = run(t, extra)
	if err != nil || rep.Breaking() || !rep.Additive() {
		t.Fatalf("an additive error class: %v breaking=%v additive=%v", err, rep.Breaking(), rep.Additive())
	}
	// the module is down: a typed error and nothing else
	down := newStack(t, reply(200, string(good)), modclient.Options{})
	down.stub.Close()
	if rep, err := down.a.CheckVocabularies(ctxT(t)); modclient.KindOf(err) != modclient.Unavailable || len(rep.Diffs) != 0 {
		t.Fatalf("a module that is down: %v %+v", err, rep)
	}
	if d, err := down.a.Describe(ctxT(t)); modclient.KindOf(err) != modclient.Unavailable || d != nil {
		t.Fatalf("describe of a module that is down: %v %v", err, d)
	}
	if v, err := down.a.Vocabularies(ctxT(t)); modclient.KindOf(err) != modclient.Unavailable || v != nil {
		t.Fatalf("vocabularies of a module that is down: %v %v", err, v)
	}
}

// ---- the suspension read ---------------------------------------------------------------------------------------------------

// A *m3admin.Client is what the reconcile loop already holds, and it is what the adapter composes with: it must satisfy the interface as it is.
var _ m3.SuspensionReader = (*m3admin.Client)(nil)

type fakeReader struct {
	state m3admin.SuspensionState
	err   error
	calls atomic.Int32
	asked atomic.Value
}

func (f *fakeReader) Suspension(_ context.Context, id string) (m3admin.SuspensionState, error) {
	f.calls.Add(1)
	f.asked.Store(id)
	if f.err != nil {
		return m3admin.SuspensionState{}, f.err
	}
	return f.state, nil
}

func adapterWith(t testing.TB, r m3.SuspensionReader) *m3.Adapter {
	t.Helper()
	stub := sendapitest.NewStub(t, func(w http.ResponseWriter, _ *http.Request, _ int) { sendapitest.JSON(w, 500, "{}") })
	opts := []m3.Option{}
	if r != nil {
		opts = append(opts, m3.WithSuspensionReader(r))
	}
	a, err := m3.NewAdapter(sendapitest.ModClient(t, modclient.M3, stub.URL(), sendapitest.Token(t), modclient.Options{}), opts...)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// Suspension reports what module 3 was seen to say, exactly, and nothing else (CLAUDE.md rule 21): the user it was asked about, the version, the state and both instants. The reader is asked
// about the user the caller named. The control: a reader that answers for another user, or with a negative version, is refused.
func TestSuspensionIsExactlyWhatTheModuleSaid(t *testing.T) {
	at := time.Date(2026, 11, 17, 12, 0, 0, 123456000, time.UTC)
	back := at.Add(time.Hour)
	for name, st := range map[string]m3admin.SuspensionState{
		"suspended":    {User: userA, Suspended: true, SuspendedAt: &at, Version: 3},
		"reinstated":   {User: userA, Suspended: false, SuspendedAt: &at, ReinstatedAt: &back, Version: 4},
		"never":        {User: userA, Version: 0},
		"suspended v1": {User: userA, Suspended: true, SuspendedAt: &at, Version: 1},
	} {
		f := &fakeReader{state: st}
		got, err := adapterWith(t, f).Suspension(ctxT(t), userA)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.UserID != st.User || got.Suspended != st.Suspended || got.Version != st.Version || got.SuspendedAt != st.SuspendedAt || got.ReinstatedAt != st.ReinstatedAt {
			t.Fatalf("%s: got %+v from %+v", name, got, st)
		}
		if f.asked.Load() != userA || f.calls.Load() != 1 {
			t.Fatalf("%s: the reader was asked %v %d times", name, f.asked.Load(), f.calls.Load())
		}
	}
	// control: an answer for another user, or a negative counter, is refused with the key it names and no state
	for name, tc := range map[string]struct {
		st  m3admin.SuspensionState
		key string
	}{
		"another user":     {m3admin.SuspensionState{User: userB, Suspended: true, Version: 2}, "user_id"},
		"negative version": {m3admin.SuspensionState{User: userA, Version: -1}, "version"},
	} {
		got, err := adapterWith(t, &fakeReader{state: tc.st}).Suspension(ctxT(t), userA)
		if ce := wantKind(t, err, modclient.Contract); ce.Key != tc.key || !isZero(got) {
			t.Fatalf("%s: %v %+v", name, err, got)
		}
	}
}

// Suspension answers nothing it was not told: no reader wired, an id that cannot be a tenant, and every failure of the reader are typed errors with a zero state, of the kind that says what
// happened, and the original error stays reachable.
func TestSuspensionRefusesToAnswerFromAnythingElse(t *testing.T) {
	got, err := adapterWith(t, nil).Suspension(ctxT(t), userA)
	if !errors.Is(err, m3.ErrNoSuspensionReader) || modclient.KindOf(err) != modclient.Invalid || !isZero(got) {
		t.Fatalf("no reader: %v %+v", err, got)
	}
	f := &fakeReader{state: m3admin.SuspensionState{User: userA}}
	a := adapterWith(t, f)
	for id, is := range map[string]error{"": tenant.ErrBadID, tenant.SystemUserID: tenant.ErrSystemUser, strings.ToUpper(userA): tenant.ErrBadID, "u1": tenant.ErrBadID} {
		got, err := a.Suspension(ctxT(t), id)
		if !errors.Is(err, is) || modclient.KindOf(err) != modclient.Invalid || !isZero(got) {
			t.Fatalf("%q: %v %+v", id, err, got)
		}
	}
	if f.calls.Load() != 0 {
		t.Fatal("the reader was asked about something that is not a tenant")
	}
	retry := &m3admin.RetryableError{Op: "suspension read", HTTPStatus: 503, Reason: "a server error"}
	for name, tc := range map[string]struct {
		err  error
		kind modclient.Kind
		is   error
	}{
		"unknown user":   {m3admin.ErrUnknownUser, modclient.NotFound, m3admin.ErrUnknownUser},
		"wrong key":      {m3admin.ErrUnauthorized, modclient.Refused, m3admin.ErrUnauthorized},
		"invalid":        {m3admin.ErrInvalid, modclient.Invalid, m3admin.ErrInvalid},
		"retryable":      {retry, modclient.Unavailable, retry},
		"deadline":       {context.DeadlineExceeded, modclient.Timeout, context.DeadlineExceeded},
		"canceled":       {context.Canceled, modclient.Canceled, context.Canceled},
		"something else": {errors.New("a surprise"), modclient.Unavailable, nil},
	} {
		got, err := adapterWith(t, &fakeReader{err: tc.err}).Suspension(ctxT(t), userA)
		if modclient.KindOf(err) != tc.kind || !isZero(got) {
			t.Fatalf("%s: %v %+v", name, err, got)
		}
		if tc.is != nil && !errors.Is(err, tc.is) {
			t.Fatalf("%s: the original error is not reachable: %v", name, err)
		}
		if strings.Contains(err.Error(), "a surprise") {
			t.Fatalf("%s: the reader's own text reached the error", name)
		}
	}
	// an unknown user is NOT "not suspended": the one error that looks like an answer is the one that must not be one
	got, err = adapterWith(t, &fakeReader{err: m3admin.ErrUnknownUser}).Suspension(ctxT(t), userA)
	if err == nil || got.Suspended || got.Version != 0 || got.UserID != "" {
		t.Fatalf("an unknown user read as a state: %v %+v", err, got)
	}
}

// The adapter composes with the REAL m3admin client: its suspension read, over HTTP, with the operator's key (never the service token), answers exactly what the module's route said, and a
// 404 from module 3 is NotFound, never an "active" user.
func TestSuspensionThroughTheRealAdminClient(t *testing.T) {
	admin := sendapitest.Token(t)
	var answer atomic.Value
	answer.Store(`{"user_id":"` + userA + `","suspended":true,"suspended_at":"2026-11-17T12:00:00.123456Z","reinstated_at":null,"version":3}`)
	status := atomic.Int32{}
	status.Store(200)
	stub := sendapitest.NewStub(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		if status.Load() == 404 {
			sendapitest.JSON(w, 404, sendapitest.Envelope("not_found"))
			return
		}
		sendapitest.JSON(w, 200, answer.Load().(string))
	})
	ac, err := m3admin.New(stub.URL(), config.Secret(admin), nil)
	if err != nil {
		t.Fatal(err)
	}
	var a *m3.Adapter
	a, err = m3.NewAdapter(sendapitest.ModClient(t, modclient.M3, stub.URL(), sendapitest.Token(t), modclient.Options{}), m3.WithSuspensionReader(ac))
	if err != nil {
		t.Fatal(err)
	}
	got, err := a.Suspension(ctxT(t), userA)
	if err != nil || !got.Suspended || got.Version != 3 || got.UserID != userA || got.SuspendedAt == nil || got.ReinstatedAt != nil {
		t.Fatalf("%v %+v", err, got)
	}
	reqs := stub.Requests()
	if len(reqs) != 1 || reqs[0].Path != "/v1/admin/users/"+userA+"/suspension" || reqs[0].Method != http.MethodGet {
		t.Fatalf("requests: %+v", reqs)
	}
	if problems := authProblems(reqs, admin, ""); len(problems) != 0 {
		t.Fatalf("the read did not carry the operator's key: %v", problems)
	}
	status.Store(404)
	got, err = a.Suspension(ctxT(t), userA)
	if !errors.Is(err, m3admin.ErrUnknownUser) || modclient.KindOf(err) != modclient.NotFound || !isZero(got) {
		t.Fatalf("a 404: %v %+v", err, got)
	}
	status.Store(200)
	answer.Store(`{"user_id":"` + userB + `","suspended":false,"suspended_at":null,"reinstated_at":null,"version":0}`)
	if got, err := a.Suspension(ctxT(t), userA); err == nil || !isZero(got) {
		t.Fatalf("an answer for another user was accepted: %v %+v", err, got)
	}
}

// The account-side closed sets are compared with what describe publishes under the same names: the recorded describe matches exactly; a value the module drops, or a set it drops, is a
// Contract error naming the vocabulary; a value the module adds is in the diff and is not an error (the adapter maps it to Unrecognised, and this is where it is seen); a module that is down
// is a typed error and no diff.
func TestCheckAccountVocabulariesAgainstModule3sDescribe(t *testing.T) {
	good := sendapitest.Read(t, fixtures, "describe.ok.json")
	run := func(t *testing.T, body []byte) ([]modclient.VocabDiff, error) {
		return newStack(t, reply(200, string(body)), modclient.Options{}).a.CheckAccountVocabularies(ctxT(t))
	}
	diffs, err := run(t, good)
	if err != nil || len(diffs) != 6 {
		t.Fatalf("the recorded describe: %v %+v", err, diffs)
	}
	for _, d := range diffs {
		if !d.Exact() {
			t.Fatalf("%s is not exact: %+v", d.Name, d)
		}
	}
	mut := func(path string, op sendapitest.Op, v any) []byte {
		b, err := sendapitest.Mutate(good, path, op, v)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	for name, tc := range map[string]struct {
		body []byte
		key  string
		msg  string
	}{
		"a value is dropped":      {mut("vocabularies.pause_reason.values[2]", sendapitest.Delete, nil), "vocabularies.pause_reason", "manual"},
		"a quote reason is gone":  {mut("vocabularies.quote_reason_code.values[0]", sendapitest.Delete, nil), "vocabularies.quote_reason_code", "dispatch_disabled"},
		"a set is gone":           {mut("vocabularies.from_verification_state", sendapitest.Delete, nil), "vocabularies.from_verification_state", "no longer advertises"},
		"another module's answer": {mut("module", sendapitest.Set, "send_managed"), "module", "wired"},
	} {
		t.Run(name, func(t *testing.T) {
			diffs, err := run(t, tc.body)
			ce := wantKind(t, err, modclient.Contract)
			if ce.Key != tc.key || !strings.Contains(ce.Message, tc.msg) || diffs != nil {
				t.Fatalf("key %q message %q diffs %v, want %q containing %q", ce.Key, ce.Message, diffs, tc.key, tc.msg)
			}
		})
	}
	added := mut("vocabularies.quarantine_reason.values", sendapitest.Set, []string{"auth_failure", "consecutive_failures", "failure_rate", "manual", "held_for_review"})
	diffs, err = run(t, added)
	if err != nil || len(diffs) != 6 {
		t.Fatalf("an added value: %v %+v", err, diffs)
	}
	seen := false
	for _, d := range diffs {
		if d.Name == "quarantine_reason" {
			seen = len(d.Extra) == 1 && d.Extra[0] == "held_for_review"
		} else if !d.Exact() {
			t.Fatalf("%s: %+v", d.Name, d)
		}
	}
	if !seen {
		t.Fatalf("the added value is not in the diff: %+v", diffs)
	}
	down := newStack(t, reply(200, string(good)), modclient.Options{})
	down.stub.Close()
	if diffs, err := down.a.CheckAccountVocabularies(ctxT(t)); modclient.KindOf(err) != modclient.Unavailable || diffs != nil {
		t.Fatalf("a module that is down: %v %v", err, diffs)
	}
}
