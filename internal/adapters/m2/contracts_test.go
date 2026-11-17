package m2_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/adapters/m2"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi/sendapitest"
	"github.com/coldmail-platform/platform/internal/charge"
	"github.com/coldmail-platform/platform/internal/modclient"
)

func clientAt(t testing.TB, url, token string) (*modclient.Client, *m2.Client) {
	t.Helper()
	mc := sendapitest.ModClient(t, modclient.M2, url, token, modclient.Options{})
	a, err := m2.NewAdapter(mc)
	if err != nil {
		t.Fatal(err)
	}
	c, err := a.For(userA)
	if err != nil {
		t.Fatal(err)
	}
	return mc, c
}

func call(f func(ctx context.Context, c *m2.Client) (any, error)) func(t testing.TB, url, token string) (any, error) {
	return func(t testing.TB, url, token string) (any, error) {
		_, c := clientAt(t, url, token)
		return f(sendapitest.Ctx(t), c)
	}
}

func del(path, key string) sendapitest.Mutation {
	return sendapitest.Mutation{Path: path, Op: sendapitest.Delete, WantKey: key}
}

func set(path string, v any, key string) sendapitest.Mutation {
	return sendapitest.Mutation{Path: path, Op: sendapitest.Set, Value: v, WantKey: key}
}

func brokenEnvelope() sendapitest.Mutation { return del("error.code", "") }

func cases() map[string]sendapitest.Case {
	run := call(func(ctx context.Context, c *m2.Client) (any, error) { return c.Run(ctx, jobItem) })
	return map[string]sendapitest.Case{
		"POST /v1/quote | ok": {
			Do: call(func(ctx context.Context, c *m2.Client) (any, error) { return c.Quote(ctx, 3) }),
			Check: func(t testing.TB, v any, err error) {
				if q := v.(sendapi.Quote); q.Cost != 3 || q.UnitPriceCredits != 1 || q.Tier != "shared" || q.Feasible != nil {
					t.Fatalf("quote = %+v", q)
				}
			},
			Mutations: []sendapitest.Mutation{del("cost", "cost"), set("cost", "3", "cost"), set("recipient_count", 4, "recipient_count"), set("cost", 4, "cost"), del("unit_price_credits", "unit_price_credits")},
		},
		"POST /v1/run | created": {
			Do: run,
			Check: func(t testing.TB, v any, err error) {
				if r := v.(sendapi.RunResult); !r.Created || r.State != "queued" || r.JobID == "" {
					t.Fatalf("run = %+v", r)
				}
			},
			Mutations: []sendapitest.Mutation{del("job_id", "job_id"), set("state", "teleported", "state"), set("state", nil, "state")},
		},
		"POST /v1/run | replay": {
			Do: run,
			Check: func(t testing.TB, v any, err error) {
				if r := v.(sendapi.RunResult); r.Created || r.State != "sent" || r.Class != sendapi.ClassTerminal {
					t.Fatalf("run = %+v", r)
				}
			},
			Mutations: []sendapitest.Mutation{set("state", "paused", "state")},
		},
		"POST /v1/run | error validation_failed": {Do: run, Kind: modclient.Refused, Code: "validation_failed", Mutations: []sendapitest.Mutation{brokenEnvelope()}},
		"POST /v1/run | error unauthenticated":   {Do: run, Kind: modclient.Refused, Code: "unauthenticated", Mutations: []sendapitest.Mutation{brokenEnvelope()}},
		"POST /v1/run | error not_found": {
			Do: run, Kind: modclient.NotFound, Code: "not_found", Mutations: []sendapitest.Mutation{brokenEnvelope()},
			Check: func(t testing.TB, v any, err error) {
				if f, ok := sendapi.NotFoundField(err); !ok || f != "job_item_id" {
					t.Fatalf("NotFoundField = %q %v", f, ok)
				}
			},
		},
		"POST /v1/run | error conflict": {
			Do: run, Kind: modclient.Refused, Code: "conflict", Mutations: []sendapitest.Mutation{brokenEnvelope()},
			Check: func(t testing.TB, v any, err error) {
				if !errors.Is(err, sendapi.ErrBackpressure) || errors.Is(err, sendapi.ErrConflictUnexplained) || !errors.Is(err, modclient.ErrRefused) {
					t.Fatalf("module 2's 409 on run: %v", err)
				}
			},
		},
		"POST /v1/run | error paused":         {Do: run, Kind: modclient.Paused, Code: "paused"},
		"POST /v1/run | error internal_error": {Do: run, Kind: modclient.Internal, Code: "internal_error"},

		"GET /v1/report | job sent": {
			Do: call(func(ctx context.Context, c *m2.Client) (any, error) { return c.ReportJob(ctx, jobItem) }),
			Check: func(t testing.TB, v any, err error) {
				it := v.(sendapi.Item)
				if it.State != "sent" || it.JobItemID != jobItem || it.Reason != nil || it.ErrorClass != nil || it.ProviderMessageID == nil || it.CompletedAt == nil || it.CompletedAt.Nanosecond() == 0 {
					t.Fatalf("item = %+v", it)
				}
			},
			Mutations: []sendapitest.Mutation{
				del("reason", "reason"), del("error_class", "error_class"), del("attempt_count", "attempt_count"), set("state", "teleported", "state"),
				set("completed_at", "soon", "completed_at"), set("reason", 7, "reason"), del("job_id", "job_id"),
			},
		},
		"GET /v1/report | job queued": {
			Do: call(func(ctx context.Context, c *m2.Client) (any, error) { return c.ReportJobByID(ctx, jobID) }),
			Check: func(t testing.TB, v any, err error) {
				it := v.(sendapi.Item)
				if it.State != "queued" || it.Class != sendapi.ClassPending || it.Reason == nil || *it.Reason != "transport_4xx" || it.ProviderMessageID != nil || it.CompletedAt != nil {
					t.Fatalf("item = %+v", it)
				}
			},
			Mutations: []sendapitest.Mutation{set("error_class", 7, "error_class"), set("attempt_count", "1", "attempt_count"), del("reason", "reason")},
		},
		"GET /v1/report | job error not_found": {
			Do: call(func(ctx context.Context, c *m2.Client) (any, error) { return c.ReportJob(ctx, jobItem) }), Kind: modclient.NotFound, Code: "not_found",
			Mutations: []sendapitest.Mutation{brokenEnvelope()},
			Check: func(t testing.TB, v any, err error) {
				if f, ok := sendapi.NotFoundField(err); !ok || f != "job_id" {
					t.Fatalf("NotFoundField = %q %v", f, ok)
				}
			},
		},
		"GET /v1/report | campaign page": {
			Do: call(func(ctx context.Context, c *m2.Client) (any, error) { return c.ReportCampaignPage(ctx, campaign, "") }),
			Check: func(t testing.TB, v any, err error) {
				p := v.(sendapi.CampaignPage)
				if len(p.Items) != 3 || p.NextCursor == "" || p.Totals.Total != 5 || p.Totals.Cancelled != 1 || p.ByAccount != nil || p.Items[2].Reason == nil || *p.Items[2].Reason != "attempts_exhausted" {
					t.Fatalf("page = %+v", p)
				}
			},
			Mutations: []sendapitest.Mutation{
				del("totals", "totals"), del("totals.cancelled", "totals.cancelled"), del("items.results[0].job_item_id", "items.results[0].job_item_id"),
				set("items.results[1].state", "teleported", "items.results[1].state"), set("items.next_cursor", 5, "items.next_cursor"), del("items.results[2].reason", "items.results[2].reason"),
				del("items.results[0].error_class", "items.results[0].error_class"), set("items.results[0].attempt_count", "0", "items.results[0].attempt_count"),
				set("items.results[1].completed_at", "later", "items.results[1].completed_at"), set("items.results[0].reason", 3, "items.results[0].reason"),
				set("campaign_id", "5a1c9e30-7d2b-4f6a-9b8e-1c3d5e7f9a99", "campaign_id"), set("items.page_size", 0, "items.page_size"), set("totals.pending", -1, "totals.pending"),
			},
		},
		"GET /v1/report | campaign last page": {
			Do: call(func(ctx context.Context, c *m2.Client) (any, error) { return c.ReportCampaignPage(ctx, campaign, "") }),
			Check: func(t testing.TB, v any, err error) {
				p := v.(sendapi.CampaignPage)
				if len(p.Items) != 2 || p.NextCursor != "" || p.Items[0].Class != sendapi.ClassPending || p.Items[1].Class != sendapi.ClassTerminal {
					t.Fatalf("page = %+v", p)
				}
			},
			Mutations: []sendapitest.Mutation{del("items.page_size", "items.page_size"), del("progress", "progress")},
		},
		"GET /v1/report | campaign empty": {
			Do: call(func(ctx context.Context, c *m2.Client) (any, error) { return c.ReportCampaign(ctx, campaign) }),
			Check: func(t testing.TB, v any, err error) {
				if r := v.(sendapi.CampaignReport); !r.NothingKnown() || !r.ItemsMatchTotal() || r.Progress.IsComplete {
					t.Fatalf("report = %+v", r)
				}
			},
			Mutations: []sendapitest.Mutation{set("totals.total", "0", "totals.total")},
		},
		"GET /v1/report | campaign error cursor_malformed": {
			Do: call(func(ctx context.Context, c *m2.Client) (any, error) {
				return c.ReportCampaignPage(ctx, campaign, "abc")
			}), Kind: modclient.Refused, Code: "validation_failed",
			Mutations: []sendapitest.Mutation{brokenEnvelope()},
			Check: func(t testing.TB, v any, err error) {
				if !errors.Is(err, sendapi.ErrBadCursor) {
					t.Fatalf("a malformed cursor: %v", err)
				}
			},
		},
		"GET /v1/report | campaign error cursor_forged": {
			Do: call(func(ctx context.Context, c *m2.Client) (any, error) {
				return c.ReportCampaignPage(ctx, campaign, "abc")
			}), Kind: modclient.Internal, Code: "internal_error",
			Check: func(t testing.TB, v any, err error) {
				if !errors.Is(err, sendapi.ErrBadCursor) {
					t.Fatalf("a forged cursor: %v", err)
				}
			},
		},
		"GET /v1/describe | ok": {
			Do: func(t testing.TB, url, token string) (any, error) {
				mc := sendapitest.ModClient(t, modclient.M2, url, token, modclient.Options{})
				a, err := m2.NewAdapter(mc)
				if err != nil {
					t.Fatal(err)
				}
				d, err := a.Describe(sendapitest.Ctx(t))
				if err != nil {
					return nil, err
				}
				return d, nil
			},
			Check: func(t testing.TB, v any, err error) {
				if d := v.(*modclient.Describe); d.Module != "send_managed" || len(d.ErrorCodes) != 7 || d.Pricing.UnitPriceCredits != 1 || d.Pricing.CostFieldsInert {
					t.Fatalf("describe = %+v", d)
				}
			},
			Mutations: []sendapitest.Mutation{del("module", "module"), set("module", "send_byo", "module"), set("api_version", "v2", "api_version"), del("vocabularies", "vocabularies"), del("error_codes", "error_codes")},
		},

		"POST /v1/cancel | ok": {
			Do: call(func(ctx context.Context, c *m2.Client) (any, error) { return c.Cancel(ctx, campaign) }),
			Check: func(t testing.TB, v any, err error) {
				if p := v.(m2.CancelPosition); p.Cancelled != 2 || p.InFlight != 1 || p.AlreadyTerminal != 0 || p.CampaignID != campaign {
					t.Fatalf("position = %+v", p)
				}
			},
			Mutations: []sendapitest.Mutation{del("cancelled", "cancelled"), set("in_flight", "1", "in_flight"), del("already_terminal", "already_terminal"), del("campaign_id", "campaign_id"), set("cancelled", -2, "cancelled")},
		},
		"POST /v1/cancel | error validation_failed": {Do: call(func(ctx context.Context, c *m2.Client) (any, error) { return c.Cancel(ctx, campaign) }), Kind: modclient.Refused, Code: "validation_failed", Mutations: []sendapitest.Mutation{brokenEnvelope()}},
	}
}

// Every response of module 2 that the adapter relies on is served from its recorded file by an httptest server and decoded through the adapter's real code (the client, the walk, the
// describe, the cancel); each recorded body conforms to the Shape its contract names; and every mutation (a required key removed, a key's type changed, a state outside the
// vocabulary, an unreadable timestamp, another campaign's answer) is refused with the key it names. The unmutated body is the control of the mutations, through the same server and call.
func TestContractsOfModule2(t *testing.T) {
	sendapitest.Suite{Dir: fixtures, Contracts: m2.Contracts(), Cases: cases()}.Run(t)
}

// No rot: the files of testdata/contract and the contracts name each other exactly once. The controls: a stray file, a missing file and a doubly named file are each caught.
func TestContractFixturesDoNotRot(t *testing.T) {
	cs := m2.Contracts()
	if problems := sendapitest.CheckNoRot(fixtures, cs); len(problems) != 0 {
		t.Fatalf("the fixtures have rotted: %v", problems)
	}
	if len(cs) != len(sendapi.Contracts(m2.Profile()))+2 {
		t.Fatalf("%d contracts: the shared ones and cancel's two", len(cs))
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
	stray := copyDir(t)
	if err := os.WriteFile(filepath.Join(stray, "stray.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if p := sendapitest.CheckNoRot(stray, cs); len(p) != 1 || !has(p, "stray.json is named by no contract") {
		t.Fatalf("a stray file: %v", p)
	}
	missing := copyDir(t)
	if err := os.Remove(filepath.Join(missing, "cancel.ok.json")); err != nil {
		t.Fatal(err)
	}
	if p := sendapitest.CheckNoRot(missing, cs); len(p) != 1 || !has(p, "names the missing file cancel.ok.json") {
		t.Fatalf("a missing file: %v", p)
	}
	twice := append([]modclient.RouteContract(nil), cs...)
	twice[len(twice)-1].Fixture = twice[len(twice)-2].Fixture
	if p := sendapitest.CheckNoRot(fixtures, twice); !has(p, "is named by 2 contracts") {
		t.Fatalf("a file named twice: %v", p)
	}
}

// The cancel contracts carry the Shape the adapter decodes with (cancelShape, one definition) and the shared ones carry the shared body's shapes for module 2's profile; the contracts name
// module 2 and nothing else, and cancel's error entry lists the error codes the adapter maps.
func TestContractsNameModule2AndItsShapes(t *testing.T) {
	cs := m2.Contracts()
	byKey := map[string]modclient.RouteContract{}
	for _, c := range cs {
		if c.Module != modclient.M2 {
			t.Fatalf("%s / %s names module %s", c.Route, c.Case, c.Module)
		}
		byKey[sendapitest.Key(c)] = c
	}
	shared := map[string]modclient.RouteContract{}
	for _, c := range sendapi.Contracts(m2.Profile()) {
		shared[sendapitest.Key(c)] = c
	}
	for k, c := range shared {
		got, ok := byKey[k]
		if !ok || !reflect.DeepEqual(got, c) {
			t.Fatalf("the shared contract %q is not carried unchanged", k)
		}
	}
	ok, bad := byKey["POST /v1/cancel | ok"], byKey["POST /v1/cancel | error validation_failed"]
	if ok.Status != 200 || bad.Status != 400 || reflect.DeepEqual(ok.Shape, bad.Shape) {
		t.Fatalf("cancel contracts: %+v %+v", ok, bad)
	}
	if !slices.Equal(bad.Vocab["error_codes"], m2.Profile().ErrorCodes) {
		t.Fatalf("cancel's error vocabulary: %v", bad.Vocab)
	}
	if !reflect.DeepEqual(bad.Shape, sendapi.ErrorShape()) {
		t.Fatal("cancel's error shape is not the shared error shape")
	}
}

// Module 2's recorded describe advertises exactly what the adapter handles (no break, nothing additive), and CheckVocabularies names each way that stops being true: a state gone, a state
// the adapter does not handle, a reason or error class gone, the reason vocabulary gone, an error code gone, another module's describe. An additive reason is reported, not raised.
func TestCheckVocabulariesAgainstModule2sDescribe(t *testing.T) {
	good := sendapitest.Read(t, fixtures, "describe.ok.json")
	run := func(t *testing.T, body []byte) (sendapi.VocabReport, error) {
		s := newStack(t, reply(200, string(body)), modclient.Options{})
		return s.a.CheckVocabularies(ctxT(t))
	}
	rep, err := run(t, good)
	if err != nil || rep.Breaking() || rep.Additive() || len(rep.Diffs) != 3 {
		t.Fatalf("the recorded describe: %v %+v", err, rep)
	}
	for _, d := range rep.Diffs {
		if !d.Exact() {
			t.Fatalf("%s is not exact: %+v", d.Name, d)
		}
	}
	vs, err := newStack(t, reply(200, string(good)), modclient.Options{}).a.Vocabularies(ctxT(t))
	if err != nil || !slices.Equal(vs["job_state"], m2.Profile().JobStates) || !slices.Equal(vs["item_reason"], m2.Profile().ItemReasons) {
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
		"cancelled is gone":          {mut("vocabularies.job_state.values[6]", sendapitest.Delete, nil), "vocabularies.job_state", "cancelled"},
		"a new state":                {mut("vocabularies.job_state.values[6]", sendapitest.Set, "paused"), "vocabularies.job_state", "cancelled"},
		"a reason is gone":           {mut("vocabularies.item_reason.values[1]", sendapitest.Delete, nil), "vocabularies.item_reason", "attempts_exhausted"},
		"the reason vocabulary goes": {mut("vocabularies.item_reason", sendapitest.Delete, nil), "vocabularies.item_reason", "no longer advertises"},
		"an error class is gone":     {mut("vocabularies.error_class.values[2]", sendapitest.Delete, nil), "vocabularies.error_class", "auth_failure"},
		"an error code is gone":      {mut("error_codes[4]", sendapitest.Delete, nil), "error_codes", "conflict"},
		"module 3's describe":        {sendapitest.Read(t, "../sendapi/testdata/contract", "describe.ok.json"), "module", "wired"},
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
	// an additive reason is not an error
	var doc map[string]any
	if err := json.Unmarshal(good, &doc); err != nil {
		t.Fatal(err)
	}
	r := doc["vocabularies"].(map[string]any)["item_reason"].(map[string]any)
	r["values"] = append(r["values"].([]any), "held_for_review")
	extra, _ := json.Marshal(doc)
	rep, err = run(t, extra)
	if err != nil || rep.Breaking() || !rep.Additive() {
		t.Fatalf("an additive reason: %v breaking=%v additive=%v", err, rep.Breaking(), rep.Additive())
	}
	// the module is down: a typed error and no report
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

// Recorded pages to settlement, offline: the two recorded pages of a campaign are walked through the real client (the first page names a cursor, the second does not) and classified
// against the platform's charge table. The result is what the table says: the sent item is charged m2_accepted (M2-01), the bounced item m2_bounced (M2-02), the item that failed with
// attempts_exhausted is free (M2-03), the item that waited for capacity is not yet, the cancelled one is free (M2-08), and nothing is unmapped.
func TestRecordedPagesSettleAsTheChargeTableSays(t *testing.T) {
	page1 := string(sendapitest.Read(t, fixtures, "report.campaign.page.json"))
	page2 := string(sendapitest.Read(t, fixtures, "report.campaign.last_page.json"))
	s := newStack(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		if r.URL.Query().Get("cursor") == "" {
			sendapitest.JSON(w, 200, page1)
			return
		}
		sendapitest.JSON(w, 200, page2)
	}, modclient.Options{})
	c := s.client(t, userA)
	rep, err := c.ReportCampaign(ctxT(t), campaign)
	if err != nil || rep.Pages != 2 || len(rep.Items) != 5 || !rep.ItemsMatchTotal() {
		t.Fatalf("%v %+v", err, rep)
	}
	got, err := c.Classify(rep.Items)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Unmapped) != 0 || len(got.NotYet) != 1 || len(got.Settle) != 4 || got.NotYet[0].State != "queued" {
		t.Fatalf("%+v", got)
	}
	want := []struct {
		rule string
		kind charge.Kind
		unit string
	}{{"M2-01", charge.Charge, "m2_accepted"}, {"M2-02", charge.Charge, "m2_bounced"}, {"M2-03", charge.Free, ""}, {"M2-08", charge.Free, ""}}
	for i, w := range want {
		d := charge.Decide(got.Settle[i].Outcome)
		if d.Rule != w.rule || d.Kind != w.kind || d.Unit != w.unit {
			t.Fatalf("settle item %d is decided by %s as %s %q, want %s %s %q", i, d.Rule, d.Kind, d.Unit, w.rule, w.kind, w.unit)
		}
	}
	// control: the attempt count the module reports (3 for the exhausted item, 0 for the first-try send) reaches no decision
	for _, it := range rep.Items {
		changed := it
		changed.AttemptCount += 5
		if c.Profile().Outcome(changed) != c.Profile().Outcome(it) {
			t.Fatal("the attempt count changes the outcome")
		}
	}
}
