package sendapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi/sendapitest"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// m3clients builds the module client and the sendapi client of a module-3-shaped profile at url.
func m3clients(t testing.TB, url, token string) (*modclient.Client, *sendapi.Client) {
	t.Helper()
	mc := sendapitest.ModClient(t, modclient.M3, url, token, modclient.Options{})
	c, err := sendapi.New(mc, m3like(), userA, sendapi.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return mc, c
}

func call(f func(ctx context.Context, c *sendapi.Client) (any, error)) func(t testing.TB, url, token string) (any, error) {
	return func(t testing.TB, url, token string) (any, error) {
		_, c := m3clients(t, url, token)
		return f(sendapitest.Ctx(t), c)
	}
}

func del(path, key string) sendapitest.Mutation {
	return sendapitest.Mutation{Path: path, Op: sendapitest.Delete, WantKey: key}
}

func set(path string, v any, key string) sendapitest.Mutation {
	return sendapitest.Mutation{Path: path, Op: sendapitest.Set, Value: v, WantKey: key}
}

// brokenEnvelope is the damage to an error body: it claims to be the module's envelope and is not one, which modclient judges a Contract error before any key.
func brokenEnvelope() sendapitest.Mutation { return del("error.code", "") }

func m3Cases() map[string]sendapitest.Case {
	return map[string]sendapitest.Case{
		"POST /v1/quote | ok": {
			Do: call(func(ctx context.Context, c *sendapi.Client) (any, error) { return c.Quote(ctx, 50) }),
			Check: func(t testing.TB, v any, err error) {
				if q := v.(sendapi.Quote); q.Feasible == nil || !*q.Feasible || q.Cost != 0 {
					t.Fatalf("quote = %+v", q)
				}
			},
			Mutations: []sendapitest.Mutation{del("cost", "cost"), set("cost", "3", "cost"), set("recipient_count", 51, "recipient_count"), del("feasible", "feasible"), set("reasons", "none", "reasons"), del("tier", "tier")},
		},
		"POST /v1/run | created": {
			Do: call(func(ctx context.Context, c *sendapi.Client) (any, error) { return c.Run(ctx, jobItem) }),
			Check: func(t testing.TB, v any, err error) {
				if r := v.(sendapi.RunResult); !r.Created || r.State != "queued" || r.Class != sendapi.ClassPending {
					t.Fatalf("run = %+v", r)
				}
			},
			Mutations: []sendapitest.Mutation{del("job_id", "job_id"), set("state", 5, "state"), set("state", "teleported", "state"), set("job_id", "  ", "job_id")},
		},
		"POST /v1/run | replay": {
			Do: call(func(ctx context.Context, c *sendapi.Client) (any, error) { return c.Run(ctx, jobItem) }),
			Check: func(t testing.TB, v any, err error) {
				if r := v.(sendapi.RunResult); r.Created || r.State != "sent" || r.Class != sendapi.ClassTerminal {
					t.Fatalf("run = %+v", r)
				}
			},
			Mutations: []sendapitest.Mutation{set("state", "teleported", "state"), set("state", 7, "state")},
		},
		"POST /v1/run | error validation_failed": {
			Do:        call(func(ctx context.Context, c *sendapi.Client) (any, error) { return c.Run(ctx, jobItem) }),
			Kind:      modclient.Refused,
			Code:      "validation_failed",
			Mutations: []sendapitest.Mutation{brokenEnvelope()},
		},
		"POST /v1/run | error unauthenticated": {
			Do:        call(func(ctx context.Context, c *sendapi.Client) (any, error) { return c.Run(ctx, jobItem) }),
			Kind:      modclient.Refused,
			Code:      "unauthenticated",
			Mutations: []sendapitest.Mutation{brokenEnvelope()},
		},
		"POST /v1/run | error not_found": {
			Do:   call(func(ctx context.Context, c *sendapi.Client) (any, error) { return c.Run(ctx, jobItem) }),
			Kind: modclient.NotFound,
			Code: "not_found",
			Check: func(t testing.TB, v any, err error) {
				if f, ok := sendapi.NotFoundField(err); !ok || f != "job_item_id" {
					t.Fatalf("NotFoundField = %q %v", f, ok)
				}
			},
			Mutations: []sendapitest.Mutation{brokenEnvelope()},
		},
		"POST /v1/run | error conflict": {
			Do:   call(func(ctx context.Context, c *sendapi.Client) (any, error) { return c.Run(ctx, jobItem) }),
			Kind: modclient.Refused,
			Code: "conflict",
			Check: func(t testing.TB, v any, err error) {
				// the recorded 409 names two causes: another campaign is active and the queue holds its cap
				got := sendapi.CausesOf(err)
				if !slices.Equal(got, []sendapi.Cause{sendapi.CauseCampaignActive, sendapi.CauseQueueCap}) || !errors.Is(err, sendapi.ErrRunCampaignActive) || !errors.Is(err, sendapi.ErrRunQueueCap) ||
					errors.Is(err, sendapi.ErrRunSuspended) || errors.Is(err, sendapi.ErrRunCampaignCap) || errors.Is(err, sendapi.ErrConflictUnexplained) || errors.Is(err, sendapi.ErrBackpressure) {
					t.Fatalf("module 3's 409 on run: causes %v, error %v", got, err)
				}
			},
			Mutations: []sendapitest.Mutation{
				brokenEnvelope(),
				set("error.details[0].field", "teleported", "error.details[].field"),      // a cause outside the closed set
				set("error.details[1].field", "campaign_active", "error.details[].field"), // one cause named twice
				set("error.details[0].issue", "required", "error.details[].field"),        // an entry that is not a conflict
			},
		},
		"POST /v1/cancel | ok": {
			Do: call(func(ctx context.Context, c *sendapi.Client) (any, error) { return c.Cancel(ctx, campaign) }),
			Check: func(t testing.TB, v any, err error) {
				if p := v.(sendapi.CancelPosition); p.CampaignID != campaign || p.Cancelled != 7 || p.InFlight != 1 || p.AlreadyTerminal != 2 || p.Total() != 10 || p.Empty() {
					t.Fatalf("cancel = %+v", p)
				}
			},
			Mutations: []sendapitest.Mutation{del("cancelled", "cancelled"), set("in_flight", -1, "in_flight"), set("already_terminal", "2", "already_terminal"), set("cancelled", -3, "cancelled"), del("campaign_id", "campaign_id")},
		},
		"POST /v1/cancel | error validation_failed": {
			Do:        call(func(ctx context.Context, c *sendapi.Client) (any, error) { return c.Cancel(ctx, campaign) }),
			Kind:      modclient.Refused,
			Code:      "validation_failed",
			Mutations: []sendapitest.Mutation{brokenEnvelope()},
		},
		"POST /v1/run | error paused": {
			Do:   call(func(ctx context.Context, c *sendapi.Client) (any, error) { return c.Run(ctx, jobItem) }),
			Kind: modclient.Paused,
			Code: "paused",
		},
		"POST /v1/run | error internal_error": {
			Do:   call(func(ctx context.Context, c *sendapi.Client) (any, error) { return c.Run(ctx, jobItem) }),
			Kind: modclient.Internal,
			Code: "internal_error",
		},

		"GET /v1/report | job sent": {
			Do: call(func(ctx context.Context, c *sendapi.Client) (any, error) { return c.ReportJob(ctx, jobItem) }),
			Check: func(t testing.TB, v any, err error) {
				it := v.(sendapi.Item)
				if it.State != "sent" || it.AccountID == nil || it.ProviderMessageID == nil || it.CompletedAt == nil || it.ErrorClass != nil {
					t.Fatalf("item = %+v", it)
				}
			},
			Mutations: []sendapitest.Mutation{del("attempt_count", "attempt_count"), set("state", "teleported", "state"), set("completed_at", 5, "completed_at"), set("completed_at", "soon", "completed_at"), del("job_id", "job_id")},
		},
		"GET /v1/report | job queued": {
			Do: call(func(ctx context.Context, c *sendapi.Client) (any, error) { return c.ReportJobByID(ctx, jobID) }),
			Check: func(t testing.TB, v any, err error) {
				it := v.(sendapi.Item)
				if it.State != "queued" || it.Class != sendapi.ClassPending || it.SMTPResponse == nil || it.CompletedAt != nil {
					t.Fatalf("item = %+v", it)
				}
			},
			Mutations: []sendapitest.Mutation{set("error_class", 7, "error_class"), set("smtp_response", []any{}, "smtp_response"), set("attempt_count", "1", "attempt_count")},
		},
		"GET /v1/report | job error not_found": {
			Do:   call(func(ctx context.Context, c *sendapi.Client) (any, error) { return c.ReportJob(ctx, jobItem) }),
			Kind: modclient.NotFound,
			Code: "not_found",
			Check: func(t testing.TB, v any, err error) {
				if f, ok := sendapi.NotFoundField(err); !ok || f != "job_id" {
					t.Fatalf("NotFoundField = %q %v", f, ok)
				}
			},
			Mutations: []sendapitest.Mutation{brokenEnvelope()},
		},
		"GET /v1/report | campaign page": {
			Do: call(func(ctx context.Context, c *sendapi.Client) (any, error) {
				return c.ReportCampaignPage(ctx, campaign, "")
			}),
			Check: func(t testing.TB, v any, err error) {
				p := v.(sendapi.CampaignPage)
				if len(p.Items) != 3 || p.NextCursor == "" || p.Totals.Total != 5 || len(p.ByAccount) != 1 || p.Items[1].SMTPResponse == nil {
					t.Fatalf("page = %+v", p)
				}
			},
			Mutations: []sendapitest.Mutation{
				del("totals", "totals"), del("totals.waiting_for_capacity", "totals.waiting_for_capacity"), del("items.results[0].job_item_id", "items.results[0].job_item_id"),
				set("items.results[1].state", "teleported", "items.results[1].state"), set("items.next_cursor", 5, "items.next_cursor"), del("totals_by_account", "totals_by_account"),
				set("items.results[0].attempt_count", "1", "items.results[0].attempt_count"), set("items.results[2].failed_at", "someday", "items.results[2].failed_at"),
				del("items.results[0].smtp_response", "items.results[0].smtp_response"), set("progress.is_complete", "no", "progress.is_complete"), del("items", "items"),
			},
		},
		"GET /v1/report | campaign last page": {
			Do: call(func(ctx context.Context, c *sendapi.Client) (any, error) {
				return c.ReportCampaignPage(ctx, campaign, "")
			}),
			Check: func(t testing.TB, v any, err error) {
				if p := v.(sendapi.CampaignPage); len(p.Items) != 2 || p.NextCursor != "" {
					t.Fatalf("page = %+v", p)
				}
			},
			Mutations: []sendapitest.Mutation{del("items.page_size", "items.page_size"), del("items.next_cursor", "items.next_cursor"), del("dispatch_paused", "dispatch_paused")},
		},
		"GET /v1/report | campaign empty": {
			Do: call(func(ctx context.Context, c *sendapi.Client) (any, error) { return c.ReportCampaign(ctx, campaign) }),
			Check: func(t testing.TB, v any, err error) {
				if r := v.(sendapi.CampaignReport); !r.NothingKnown() || r.Pages != 1 {
					t.Fatalf("report = %+v", r)
				}
			},
			Mutations: []sendapitest.Mutation{set("totals.total", "0", "totals.total"), set("items.results", nil, "items.results")},
		},
		"GET /v1/report | campaign error cursor_malformed": {
			Do: call(func(ctx context.Context, c *sendapi.Client) (any, error) {
				return c.ReportCampaignPage(ctx, campaign, "abc")
			}),
			Kind: modclient.Refused,
			Code: "validation_failed",
			Check: func(t testing.TB, v any, err error) {
				if !errors.Is(err, sendapi.ErrBadCursor) {
					t.Fatalf("a malformed cursor: %v", err)
				}
			},
			Mutations: []sendapitest.Mutation{brokenEnvelope()},
		},
		"GET /v1/describe | ok": {
			Do: func(t testing.TB, url, token string) (any, error) {
				mc, _ := m3clients(t, url, token)
				d, err := sendapi.Describe(sendapitest.Ctx(t), mc, m3like())
				if err != nil {
					return nil, err
				}
				return d, nil
			},
			Check: func(t testing.TB, v any, err error) {
				if d := v.(*modclient.Describe); d.Module != "send_byo" || len(d.ErrorCodes) != 9 {
					t.Fatalf("describe = %+v", d)
				}
			},
			Mutations: []sendapitest.Mutation{del("module", "module"), set("module", "send_managed", "module"), set("api_version", "v2", "api_version"), del("error_codes", "error_codes"), del("pricing.currency", "pricing.currency")},
		},
	}
}

// Every contract of the module-3-shaped profile is served from an httptest server from its recorded file and decoded through the client's real code, and every mutation is refused with the
// key it names (a required key removed, a key's type changed, a state outside the vocabulary, a value that cannot be read). The recorded body with no mutation is the control of the
// mutations: it passes through the same server and the same call.
func TestContractsOfTheSharedBody(t *testing.T) {
	sendapitest.Suite{Dir: fixtures, Contracts: sendapi.Contracts(m3like()), Cases: m3Cases()}.Run(t)
}

// The paired control of the suite: with one mutation turned into an acceptable change the suite's assertion fails, so a mutation that is not refused cannot pass unseen. Here a lax
// decoder (json.Unmarshal into the struct, as an adapter without the Shape would) accepts the very bodies the adapter refuses, and what it yields is the invented zero the Shape exists to
// prevent.
func TestAMutationIsRefusedOnlyBecauseOfTheShape(t *testing.T) {
	body := sendapitest.Read(t, fixtures, "quote.ok.json")
	for _, m := range []sendapitest.Mutation{del("cost", "cost"), set("cost", nil, "cost")} {
		bad, err := sendapitest.Mutate(body, m.Path, m.Op, m.Value)
		if err != nil {
			t.Fatal(err)
		}
		var lax struct {
			Cost int64 `json:"cost"`
		}
		if err := json.Unmarshal(bad, &lax); err != nil || lax.Cost != 0 {
			t.Fatalf("the lax decoder did not invent a zero: %v %+v", err, lax)
		}
		e := newEnv(t, m3like(), userA, serve(200, bad), modclient.Options{}, sendapi.Options{})
		q, err := e.c.Quote(ctxT(t), 50)
		if ce := wantKind(t, err, modclient.Contract); ce.Key != "cost" || !isZero(q) {
			t.Fatalf("the adapter: key %q, quote %+v", ce.Key, q)
		}
	}
	// the mutator itself refuses a path that does not exist (a mutation that changes nothing would let a test pass for no reason)
	for _, path := range []string{"no_such_key", "items.results[0].x", "totals.total.x", "bad path!"} {
		if _, err := sendapitest.Mutate(body, path, sendapitest.Delete, nil); err == nil {
			t.Fatalf("a mutation of the missing path %q was accepted", path)
		}
	}
}

// No rot (b): every file under testdata/contract is named by exactly one contract and every contract names an existing file. The controls run the same check on a directory with a stray
// file, a missing file, a file named twice, and on empty subjects: each is caught for its own reason.
func TestContractFixturesDoNotRot(t *testing.T) {
	cs := sendapi.Contracts(m3like())
	if problems := sendapitest.CheckNoRot(fixtures, cs); len(problems) != 0 {
		t.Fatalf("the fixtures have rotted: %v", problems)
	}
	copyDir := func(t *testing.T) string {
		dir := t.TempDir()
		entries, err := os.ReadDir(fixtures)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			b, err := os.ReadFile(filepath.Join(fixtures, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, e.Name()), b, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	has := func(problems []string, sub string) bool {
		for _, p := range problems {
			if strings.Contains(p, sub) {
				return true
			}
		}
		return false
	}
	t.Run("a stray file", func(t *testing.T) {
		dir := copyDir(t)
		if err := os.WriteFile(filepath.Join(dir, "stray.json"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if p := sendapitest.CheckNoRot(dir, cs); len(p) != 1 || !has(p, "stray.json is named by no contract") {
			t.Fatalf("%v", p)
		}
	})
	t.Run("a missing file", func(t *testing.T) {
		dir := copyDir(t)
		if err := os.Remove(filepath.Join(dir, "quote.ok.json")); err != nil {
			t.Fatal(err)
		}
		if p := sendapitest.CheckNoRot(dir, cs); len(p) != 1 || !has(p, "names the missing file quote.ok.json") {
			t.Fatalf("%v", p)
		}
	})
	t.Run("a file named twice", func(t *testing.T) {
		twice := append([]modclient.RouteContract(nil), cs...)
		twice[1].Fixture = twice[0].Fixture
		p := sendapitest.CheckNoRot(fixtures, twice)
		if !has(p, "is named by 2 contracts") || !has(p, "is named by no contract") {
			t.Fatalf("%v", p)
		}
	})
	t.Run("two contracts with the same route and case", func(t *testing.T) {
		dup := append(append([]modclient.RouteContract(nil), cs...), cs[0])
		if p := sendapitest.CheckNoRot(fixtures, dup); !has(p, "same route and case") {
			t.Fatalf("%v", p)
		}
	})
	t.Run("a fixture name that is a path", func(t *testing.T) {
		path := append([]modclient.RouteContract(nil), cs...)
		path[0].Fixture = "../contract/quote.ok.json"
		if p := sendapitest.CheckNoRot(fixtures, path); !has(p, "no plain file name") {
			t.Fatalf("%v", p)
		}
	})
	t.Run("empty subjects", func(t *testing.T) {
		if p := sendapitest.CheckNoRot(fixtures, nil); !has(p, "no contracts") {
			t.Fatalf("%v", p)
		}
		if p := sendapitest.CheckNoRot(t.TempDir(), cs); !has(p, "no fixture files") {
			t.Fatalf("%v", p)
		}
		if p := sendapitest.CheckNoRot(filepath.Join(t.TempDir(), "absent"), cs); !has(p, "cannot be read") {
			t.Fatalf("%v", p)
		}
	})
}

// The Shape of every contract is the Shape the client decodes that response with: the client's shape set and the contracts' are built by the same code, and this holds them equal. The
// control compares module 3's contracts with module 2's client, which must differ (the profiles differ in shape), so the comparison can fail.
func TestContractShapesAreTheClientsShapes(t *testing.T) {
	shapeName := func(c modclient.RouteContract) string {
		switch {
		case c.Route == sendapi.RouteRun && c.Case == "error conflict":
			return "conflict"
		case c.Status >= 400:
			return "error"
		case c.Route == sendapi.RouteCancel:
			return "cancel"
		case c.Route == sendapi.RouteQuote:
			return "quote"
		case c.Route == sendapi.RouteRun:
			return "run"
		case c.Route == sendapi.RouteDescribe:
			return "describe"
		case strings.HasPrefix(c.Case, "job"):
			return "job"
		}
		return "page"
	}
	for name, p := range profiles() {
		c := newEnv(t, p, userA, nil, modclient.Options{}, sendapi.Options{}).c
		shapes := sendapi.ClientShapesForTest(c)
		contracts := sendapi.Contracts(p)
		if len(contracts) == 0 {
			t.Fatalf("%s: no contracts", name)
		}
		seen := map[string]bool{}
		for _, ct := range contracts {
			n := shapeName(ct)
			seen[n] = true
			if !reflect.DeepEqual(ct.Shape, shapes[n]) {
				t.Fatalf("%s: the shape of %s / %s is not the client's %s shape", name, ct.Route, ct.Case, n)
			}
		}
		for n := range shapes {
			if !seen[n] {
				t.Fatalf("%s: no contract carries the client's %s shape", name, n)
			}
		}
	}
	m2c := newEnv(t, m2like(), userA, nil, modclient.Options{}, sendapi.Options{}).c
	m2shapes := sendapi.ClientShapesForTest(m2c)
	for _, ct := range sendapi.Contracts(m3like()) {
		if ct.Route == sendapi.RouteReport && ct.Case == "campaign page" && reflect.DeepEqual(ct.Shape, m2shapes["page"]) {
			t.Fatal("module 3's campaign page shape equals module 2's: the comparison cannot fail")
		}
		if ct.Route == sendapi.RouteQuote && reflect.DeepEqual(ct.Shape, m2shapes["quote"]) {
			t.Fatal("module 3's quote shape equals module 2's: the comparison cannot fail")
		}
	}
}

// A profile that fails validation has no contracts (an empty subject is red wherever it is counted), and the contracts of the two modules differ where the modules differ: the same route
// lists the same cases, and the vocabularies of module 3 have no item_reason.
func TestContractsOfAnInvalidProfileAreEmpty(t *testing.T) {
	if cs := sendapi.Contracts(sendapi.Profile{}); cs != nil {
		t.Fatalf("%d contracts for an empty profile", len(cs))
	}
	m2, m3 := sendapi.Contracts(m2like()), sendapi.Contracts(m3like())
	if len(m2) != len(m3) || len(m2) == 0 {
		t.Fatalf("%d and %d contracts", len(m2), len(m3))
	}
	for i := range m2 {
		if m2[i].Route != m3[i].Route || m2[i].Case != m3[i].Case || m2[i].Fixture != m3[i].Fixture || m2[i].Status != m3[i].Status {
			t.Fatalf("the contracts of the two modules differ at %d: %+v and %+v", i, m2[i], m3[i])
		}
		if m2[i].Module != modclient.M2 || m3[i].Module != modclient.M3 {
			t.Fatalf("the contracts name the wrong module at %d", i)
		}
	}
}
