package m3_test

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/adapters/m3"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi/sendapitest"
	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/modclient"
)

func clientAt(t testing.TB, url, token string) *m3.Client {
	t.Helper()
	mc := sendapitest.ModClient(t, modclient.M3, url, token, modclient.Options{})
	a, err := m3.NewAdapter(mc)
	if err != nil {
		t.Fatal(err)
	}
	c, err := a.For(userA)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func call(f func(ctx context.Context, c *m3.Client, t testing.TB) (any, error)) func(t testing.TB, url, token string) (any, error) {
	return func(t testing.TB, url, token string) (any, error) {
		return f(sendapitest.Ctx(t), clientAt(t, url, token), t)
	}
}

func del(path, key string) sendapitest.Mutation {
	return sendapitest.Mutation{Path: path, Op: sendapitest.Delete, WantKey: key}
}

func set(path string, v any, key string) sendapitest.Mutation {
	return sendapitest.Mutation{Path: path, Op: sendapitest.Set, Value: v, WantKey: key}
}

func brokenEnvelope() sendapitest.Mutation { return del("error.code", "") }

// newAcct is an account to add: the secret is made at run time and never printed.
func newAcct(t testing.TB, n int) m3.NewAccount {
	t.Helper()
	name := "sender-" + strconv.Itoa(n) + "@example.test"
	return m3.NewAccount{SMTPHost: "smtp.example.test", SMTPUsername: name, SMTPSecret: config.Secret(sendapitest.Token(t)), FromAddress: name}
}

func batch(t testing.TB, n int) []m3.NewAccount {
	t.Helper()
	out := make([]m3.NewAccount, n)
	for i := range out {
		out[i] = newAcct(t, i+1)
	}
	return out
}

func detailFields(err error) []string {
	var e *modclient.Error
	if !errors.As(err, &e) {
		return nil
	}
	var out []string
	for _, d := range e.Details {
		out = append(out, d.Field+"/"+d.Issue)
	}
	return out
}

func cases(t testing.TB) map[string]sendapitest.Case {
	run := call(func(ctx context.Context, c *m3.Client, t testing.TB) (any, error) { return c.Run(ctx, jobItem) })
	other := userB
	// what the recordings say, read from the recordings: the recipient counts the quotes were asked for, and the documents the checks hold the decoded values to
	quoteOK, quoteInf := doc(t, "quote.ok.json"), doc(t, "quote.infeasible.json")
	quoteN, quoteInfN := asInt(t, quoteOK, "recipient_count"), asInt(t, quoteInf, "recipient_count")
	reportSent, reportNF := doc(t, "report.job.sent.json"), doc(t, "report.job.error.not_found.json")
	campaignPage, campaignEmpty := doc(t, "report.campaign.page.json"), doc(t, "report.campaign.empty.json")
	listPage, listLast, listEmpty := doc(t, "accounts.list.page.json"), doc(t, "accounts.list.empty.json"), doc(t, "accounts.list.last_page.json")
	listErrValidation, listErrCursor := doc(t, "accounts.list.error.validation_failed.json"), doc(t, "accounts.list.error.cursor_malformed.json")
	createErrValidation, createErrConflict := doc(t, "accounts.create.error.validation_failed.json"), doc(t, "accounts.create.error.conflict.json")
	runCreated, runReplay, reportQueued := doc(t, "run.created.json"), doc(t, "run.replay.json"), doc(t, "report.job.queued.json")
	campaignLast, describeDoc, quoteFeasible := doc(t, "report.campaign.last_page.json"), doc(t, "describe.ok.json"), doc(t, "quote.feasible.json")
	feasibleN := asInt(t, quoteFeasible, "recipient_count")
	conflictDoc, cancelDoc := doc(t, "run.error.conflict.json"), doc(t, "cancel.ok.json")
	getOK, created, createdFailed := doc(t, "accounts.get.ok.json"), doc(t, "accounts.create.created.json"), doc(t, "accounts.create.created_failed.json")
	dupBody, testOK, testFailed, bulkOK := doc(t, "accounts.create.error.duplicate.json"), doc(t, "accounts.test.ok.json"), doc(t, "accounts.test.failed.json"), doc(t, "accounts.bulk.ok.json")
	bulkN := length(t, bulkOK, "results")
	return map[string]sendapitest.Case{
		// ---- the shared four routes and describe, with module 3's profile
		"POST /v1/quote | ok": {
			Do: call(func(ctx context.Context, c *m3.Client, t testing.TB) (any, error) { return c.Quote(ctx, quoteN) }),
			Check: func(t testing.TB, v any, err error) {
				q := v.(sendapi.Quote)
				same(t, "recipient_count", q.RecipientCount, quoteN)
				same(t, "cost", q.Cost, asInt(t, quoteOK, "cost"))
				same(t, "unit_price_credits", q.UnitPriceCredits, asInt(t, quoteOK, "unit_price_credits"))
				same(t, "currency", q.Currency, asStr(t, quoteOK, "currency"))
				same(t, "tier", q.Tier, asStr(t, quoteOK, "tier"))
				if q.Feasible == nil || *q.Feasible != must(t, quoteOK, "feasible").(bool) || len(q.InfeasibleReasons) != length(t, quoteOK, "reasons") {
					t.Fatalf("quote = %+v", q)
				}
			},
			Mutations: []sendapitest.Mutation{
				del("cost", "cost"), set("recipient_count", quoteN+1, "recipient_count"), del("feasible", "feasible"), set("feasible", "yes", "feasible"), del("reasons", "reasons"),
				set("unit_price_credits", "0", "unit_price_credits"),
			},
		},
		"POST /v1/run | created": {
			Do: run,
			Check: func(t testing.TB, v any, err error) {
				// a job just created: the module's 202, with the job id and state the recording holds
				r := v.(sendapi.RunResult)
				same(t, "job_id", r.JobID, asStr(t, runCreated, "job_id"))
				same(t, "state", r.State, asStr(t, runCreated, "state"))
				same(t, "class", r.Class, classOf(r.State))
				if !r.Created {
					t.Fatalf("run = %+v", r)
				}
			},
			Mutations: []sendapitest.Mutation{del("job_id", "job_id"), set("state", "teleported", "state"), set("state", nil, "state")},
		},
		"POST /v1/run | replay": {
			Do: run,
			Check: func(t testing.TB, v any, err error) {
				// the replay of a job that already existed: the module's 200, with the state the job has now
				r := v.(sendapi.RunResult)
				same(t, "job_id", r.JobID, asStr(t, runReplay, "job_id"))
				same(t, "state", r.State, asStr(t, runReplay, "state"))
				same(t, "class", r.Class, classOf(r.State))
				if r.Created {
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
			Do: run, Kind: modclient.Refused, Code: "conflict",
			Mutations: []sendapitest.Mutation{
				brokenEnvelope(),
				set("error.details[0].field", "teleported", "error.details[].field"), set("error.details[0].issue", "required", "error.details[].field"),
			},
			Check: func(t testing.TB, v any, err error) {
				// the causes the file names, read from the file: the typed error carries exactly those and no sentinel of another cause
				var want []sendapi.Cause
				for i := 0; i < length(t, conflictDoc, "error.details"); i++ {
					want = append(want, sendapi.Cause(asStr(t, conflictDoc, fmt.Sprintf("error.details[%d].field", i))))
				}
				if got := sendapi.CausesOf(err); len(want) == 0 || !slices.Equal(got, want) || errors.Is(err, sendapi.ErrConflictUnexplained) || errors.Is(err, sendapi.ErrBackpressure) || !errors.Is(err, modclient.ErrRefused) {
					t.Fatalf("module 3's 409 on run: causes %v, want %v: %v", got, want, err)
				}
				for _, c := range sendapi.RunCauses() {
					if errors.Is(err, c.Sentinel()) != slices.Contains(want, c) {
						t.Fatalf("errors.Is(%s) = %v for a 409 that names %v", c, errors.Is(err, c.Sentinel()), want)
					}
				}
			},
		},
		"POST /v1/cancel | ok": {
			Do: call(func(ctx context.Context, c *m3.Client, t testing.TB) (any, error) {
				return c.Cancel(ctx, asStr(t, cancelDoc, "campaign_id"))
			}),
			Check: func(t testing.TB, v any, err error) {
				p := v.(sendapi.CancelPosition)
				c, f, a := asInt(t, cancelDoc, "cancelled"), asInt(t, cancelDoc, "in_flight"), asInt(t, cancelDoc, "already_terminal")
				if p.Cancelled != c || p.InFlight != f || p.AlreadyTerminal != a || p.CampaignID != asStr(t, cancelDoc, "campaign_id") || p.Total() != c+f+a || p.Empty() != (c == 0 && f == 0 && a == 0) {
					t.Fatalf("cancel = %+v, the recording says %d, %d, %d", p, c, f, a)
				}
			},
			Mutations: []sendapitest.Mutation{del("cancelled", "cancelled"), set("in_flight", "1", "in_flight"), del("already_terminal", "already_terminal"), del("campaign_id", "campaign_id"), set("cancelled", -2, "cancelled")},
		},
		"POST /v1/cancel | error validation_failed": {
			Do:   call(func(ctx context.Context, c *m3.Client, t testing.TB) (any, error) { return c.Cancel(ctx, campaign) }),
			Kind: modclient.Refused, Code: "validation_failed", Mutations: []sendapitest.Mutation{brokenEnvelope()},
		},
		"POST /v1/run | error paused":         {Do: run, Kind: modclient.Paused, Code: "paused"},
		"POST /v1/run | error internal_error": {Do: run, Kind: modclient.Internal, Code: "internal_error"},

		"GET /v1/report | job sent": {
			Do: call(func(ctx context.Context, c *m3.Client, t testing.TB) (any, error) { return c.ReportJob(ctx, jobItem) }),
			Check: func(t testing.TB, v any, err error) {
				// the recording is a settled item that failed at the recipient: the keys the module omits until set are all here, and every one of them is carried
				it := v.(sendapi.Item)
				itemMatches(t, it, reportSent, true)
				same(t, "job item id", it.JobItemID, jobItem)
				if it.ErrorClass == nil || it.FailedAt == nil || it.SMTPResponse == nil || it.AccountID == nil || it.AccountUsername == nil || it.CompletedAt == nil {
					t.Fatalf("the recording's optional keys were not carried: %+v", it)
				}
			},
			Mutations: []sendapitest.Mutation{
				del("attempt_count", "attempt_count"), set("state", "teleported", "state"), set("completed_at", "soon", "completed_at"), del("job_id", "job_id"),
				set("account_username", 7, "account_username"), set("error_class", 7, "error_class"), set("failed_at", "yesterday", "failed_at"), set("smtp_response", 5, "smtp_response"),
				set("account_id", 5, "account_id"),
			},
		},
		"GET /v1/report | job queued": {
			Do: call(func(ctx context.Context, c *m3.Client, t testing.TB) (any, error) { return c.ReportJobByID(ctx, jobID) }),
			Check: func(t testing.TB, v any, err error) {
				// a job that has not been claimed: the module sends only the keys that are set, and the Item carries nil for every other one (what a queued job that failed an attempt
				// carries is held by TestKeysTheRecordingsLackAreReadAndRefusedWhenBroken, which injects those keys into this recording)
				it := v.(sendapi.Item)
				itemMatches(t, it, reportQueued, true)
				if it.Class != sendapi.ClassPending {
					t.Fatalf("item = %+v", it)
				}
			},
			Mutations: []sendapitest.Mutation{set("attempt_count", "1", "attempt_count"), del("attempt_count", "attempt_count"), del("job_id", "job_id"), set("state", "teleported", "state"), set("state", nil, "state")},
		},
		"GET /v1/report | job error not_found": {
			Do: call(func(ctx context.Context, c *m3.Client, t testing.TB) (any, error) { return c.ReportJob(ctx, jobItem) }), Kind: modclient.NotFound, Code: "not_found",
			Mutations: []sendapitest.Mutation{brokenEnvelope()},
			Check: func(t testing.TB, v any, err error) {
				// the module names the selector the caller sent (here the job item id), as the recording says
				if f, ok := sendapi.NotFoundField(err); !ok || f != asStr(t, reportNF, "error.details[0].field") || f != "job_item_id" {
					t.Fatalf("NotFoundField = %q %v", f, ok)
				}
			},
		},
		"GET /v1/report | campaign page": {
			Do: call(func(ctx context.Context, c *m3.Client, t testing.TB) (any, error) {
				return c.ReportCampaignPage(ctx, campaign, "")
			}),
			Check: func(t testing.TB, v any, err error) {
				p := v.(sendapi.CampaignPage)
				pageMatches(t, p, campaignPage)
				if len(p.Items) == 0 || len(p.ByAccount) == 0 {
					t.Fatalf("the recording holds no item or no per-account total: %d items, %d accounts", len(p.Items), len(p.ByAccount))
				}
			},
			Mutations: []sendapitest.Mutation{
				del("totals", "totals"), del("items.results[0].job_item_id", "items.results[0].job_item_id"), set("items.results[1].state", "teleported", "items.results[1].state"),
				set("items.next_cursor", 5, "items.next_cursor"), del("items.results[1].smtp_response", "items.results[1].smtp_response"), del("items.results[0].account_id", "items.results[0].account_id"),
				set("items.results[0].attempt_count", "0", "items.results[0].attempt_count"), set("items.results[1].completed_at", "later", "items.results[1].completed_at"),
				set("campaign_id", other, "campaign_id"), set("items.page_size", 0, "items.page_size"), set("totals.pending", -1, "totals.pending"), del("totals_by_account", "totals_by_account"),
				set("totals_by_account[0].sent", "1", "totals_by_account[0].sent"), del("items.results[2].failed_at", "items.results[2].failed_at"),
			},
		},
		"GET /v1/report | campaign last page": {
			Do: call(func(ctx context.Context, c *m3.Client, t testing.TB) (any, error) {
				return c.ReportCampaignPage(ctx, campaign, "")
			}),
			Check: func(t testing.TB, v any, err error) {
				// whatever page the recording is (see the README: it is a page with a cursor), the adapter returns exactly what it holds, the cursor included
				pageMatches(t, v.(sendapi.CampaignPage), campaignLast)
			},
			Mutations: []sendapitest.Mutation{del("items.page_size", "items.page_size"), del("progress", "progress")},
		},
		"GET /v1/report | campaign empty": {
			Do: call(func(ctx context.Context, c *m3.Client, t testing.TB) (any, error) {
				return c.ReportCampaignPage(ctx, campaign, "")
			}),
			Check: func(t testing.TB, v any, err error) {
				// one page, held to the recording (a walk would ask the same stub again for the cursor this page names, and refuse the repeat: the walk is held by the walk tests)
				pageMatches(t, v.(sendapi.CampaignPage), campaignEmpty)
			},
			Mutations: []sendapitest.Mutation{set("totals.total", "0", "totals.total")},
		},
		"GET /v1/report | campaign error cursor_malformed": {
			Do: call(func(ctx context.Context, c *m3.Client, t testing.TB) (any, error) {
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
			Do: call(func(ctx context.Context, c *m3.Client, t testing.TB) (any, error) {
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
				a, err := m3.NewAdapter(sendapitest.ModClient(t, modclient.M3, url, token, modclient.Options{}))
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
				d := v.(*modclient.Describe)
				same(t, "module", d.Module, asStr(t, describeDoc, "module"))
				same(t, "api_version", d.APIVersion, asStr(t, describeDoc, "api_version"))
				same(t, "error codes", len(d.ErrorCodes), length(t, describeDoc, "error_codes"))
				same(t, "unit price", d.Pricing.UnitPriceCredits, asInt(t, describeDoc, "pricing.unit_price_credits"))
				same(t, "model", d.Pricing.Model, asStr(t, describeDoc, "pricing.model"))
				same(t, "cost fields inert", d.Pricing.CostFieldsInert, must(t, describeDoc, "pricing.cost_fields_inert").(bool))
				same(t, "job states", len(d.Vocabularies["job_state"]), length(t, describeDoc, "vocabularies.job_state.values"))
			},
			Mutations: []sendapitest.Mutation{del("module", "module"), set("module", "send_managed", "module"), set("api_version", "v2", "api_version"), del("vocabularies", "vocabularies"), del("error_codes", "error_codes")},
		},

		// ---- the feasibility quote
		"POST /v1/quote | feasible": {
			Do: call(func(ctx context.Context, c *m3.Client, t testing.TB) (any, error) {
				return c.Feasibility(ctx, feasibleN)
			}),
			Check: func(t testing.TB, v any, err error) {
				f := v.(m3.Feasibility)
				feasibilityMatches(t, f, quoteFeasible)
				if !f.Feasible || len(f.Reasons) != 0 {
					t.Fatalf("the recording is not a feasible quote: %+v", f)
				}
			},
			Mutations: keepExisting(t, quoteFeasible, 12,
				del("caps", "caps"), del("caps.queue_depth_cap", "caps.queue_depth_cap"), set("caps.daily_send_cap", "1000", "caps.daily_send_cap"), set("caps.queue_depth_cap", -1, "caps.queue_depth_cap"),
				del("capacity", "capacity"), set("capacity.estimated_duration_sec", "x", "capacity.estimated_duration_sec"), del("capacity.estimate_basis", "capacity.estimate_basis"),
				set("capacity.eligible_accounts", -2, "capacity.eligible_accounts"), del("warnings", "warnings"), set("warnings[0].threshold_sec", "x", "warnings[0].threshold_sec"),
				set("warnings[0].code", 5, "warnings[0].code"), set("feasible", false, "feasible"), set("recipient_count", feasibleN+1, "recipient_count"),
			),
		},
		"POST /v1/quote | infeasible": {
			Do: call(func(ctx context.Context, c *m3.Client, t testing.TB) (any, error) {
				return c.Feasibility(ctx, quoteInfN)
			}),
			Check: func(t testing.TB, v any, err error) {
				f := v.(m3.Feasibility)
				// what the recording says: not feasible, a reason for each cause of a run refusal that applies (the campaign size cap, then the queue depth cap), each with its cause, the cap
				// that bound, what the cap holds and the count asked, no projection
				if f.Feasible || len(f.Reasons) != 2 || f.Capacity.EstimatedDurationSec != nil || f.Capacity.EstimateBasis != "" || len(f.Warnings) != 0 {
					t.Fatalf("feasibility = %+v", f)
				}
				for i, want := range []struct {
					code  m3.InfeasibleCode
					cause sendapi.Cause
				}{{m3.InfeasibleCampaignCap, sendapi.CauseCampaignCap}, {m3.InfeasibleExceedsQueueCap, sendapi.CauseQueueCap}} {
					r, at := f.Reasons[i], fmt.Sprintf("reasons[%d].", i)
					if r.Code != want.code || r.Cause != want.cause || r.Limit == nil || *r.Limit != asInt(t, quoteInf, at+"limit") || r.Requested == nil || *r.Requested != quoteInfN || r.Current == nil || *r.Current != asInt(t, quoteInf, at+"current") || r.CapName != asStr(t, quoteInf, at+"cap_name") {
						t.Fatalf("reason %d = %+v, want %s with cause %s and the numbers of the recording", i, r, want.code, want.cause)
					}
				}
				if got := f.Causes(); !slices.Equal(got, []sendapi.Cause{sendapi.CauseCampaignCap, sendapi.CauseQueueCap}) || !f.Predicts(sendapi.CauseQueueCap) || f.Predicts(sendapi.CauseSuspended) {
					t.Fatalf("the causes the quote predicts: %v", got)
				}
				same(t, "queue depth cap", f.Caps.QueueDepthCap, asInt(t, quoteInf, "caps.queue_depth_cap"))
				same(t, "campaign size cap", f.Caps.CampaignSizeCap, asInt(t, quoteInf, "caps.campaign_size_cap"))
				same(t, "eligible accounts", f.Capacity.EligibleAccounts, asInt(t, quoteInf, "capacity.eligible_accounts"))
			},
			Mutations: []sendapitest.Mutation{
				set("feasible", true, "feasible"), del("reasons[0].code", "reasons[0].code"), set("reasons[0].limit", "x", "reasons[0].limit"), set("reasons[0].limit", -1, "reasons[0].limit"),
				set("reasons[0].requested", 1.5, "reasons[0].requested"), del("caps.campaign_size_cap", "caps.campaign_size_cap"),
				set("reasons[0].cause", "full_moon", "reasons[0].cause"), // a cause outside the closed set
				set("reasons[0].cause", "queue_cap", "reasons[0].cause"), // a reason that names the cause of another
				set("reasons[1].current", -1, "reasons[1].limit"),        // a number that is negative
				del("reasons[1].cause", "reasons[1].cause"),              // a cap reason that names no cause
				set("reasons[1].current", "x", "reasons[1].current"),     // a number that is not one

			},
		},

		// ---- the account routes
		"GET /v1/send-byo/accounts | page": {
			Do: call(func(ctx context.Context, c *m3.Client, t testing.TB) (any, error) { return c.ListAccounts(ctx, "") }),
			Check: func(t testing.TB, v any, err error) {
				// every account of the recording is held to the document, and the cursor is the module's own (a full page of the module's page size, when the recording is one)
				accountPageMatches(t, v.(m3.AccountPage), listPage)
			},
			Mutations: listMutations(t, listPage, other),
		},
		"GET /v1/send-byo/accounts | last page": {
			Do: call(func(ctx context.Context, c *m3.Client, t testing.TB) (any, error) { return c.ListAccounts(ctx, "") }),
			Check: func(t testing.TB, v any, err error) {
				accountPageMatches(t, v.(m3.AccountPage), listEmpty)
			},
			Mutations: listMutations(t, listEmpty, other),
		},
		"GET /v1/send-byo/accounts | empty": {
			Do: call(func(ctx context.Context, c *m3.Client, t testing.TB) (any, error) { return c.ListAccounts(ctx, "") }),
			Check: func(t testing.TB, v any, err error) {
				// whatever page the recording is (see the README), the adapter returns exactly what it holds; TestAnEmptyPageThatNamesACursorIsRefused holds the empty page
				accountPageMatches(t, v.(m3.AccountPage), listLast)
			},
			Mutations: listMutations(t, listLast, other),
		},
		"GET /v1/send-byo/accounts | error validation_failed": {
			Do: call(func(ctx context.Context, c *m3.Client, t testing.TB) (any, error) { return c.ListAccounts(ctx, "") }), Kind: modclient.Refused, Code: "validation_failed", Mutations: []sendapitest.Mutation{brokenEnvelope()},
			Check: func(t testing.TB, v any, err error) {
				if d := detailFields(err); !slices.Equal(d, docDetails(t, listErrValidation)) {
					t.Fatalf("details = %v, the recording says %v", d, docDetails(t, listErrValidation))
				}
			},
		},
		"GET /v1/send-byo/accounts | error cursor_malformed": {
			Do: call(func(ctx context.Context, c *m3.Client, t testing.TB) (any, error) { return c.ListAccounts(ctx, "abc") }), Kind: modclient.Refused, Code: "validation_failed", Mutations: []sendapitest.Mutation{brokenEnvelope()},
			Check: func(t testing.TB, v any, err error) {
				if d := detailFields(err); !slices.Equal(d, docDetails(t, listErrCursor)) {
					t.Fatalf("details = %v, the recording says %v", d, docDetails(t, listErrCursor))
				}
			},
		},
		"GET /v1/send-byo/accounts/{id} | ok": {
			Do: call(func(ctx context.Context, c *m3.Client, t testing.TB) (any, error) { return c.GetAccount(ctx, acct1) }),
			Check: func(t testing.TB, v any, err error) {
				a := v.(m3.Account)
				accountMatches(t, a, getOK)
				same(t, "id", a.ID, acct1)
				same(t, "user_id", a.UserID, userA)
			},
			Mutations: []sendapitest.Mutation{
				del("id", "id"), set("user_id", other, "user_id"), set("id", acct2, "id"), del("status", "status"), set("from_name", 5, "from_name"), set("created_at", "x", "created_at"), set("daily_limit", "5", "daily_limit"),
				set("from_alignment.checked_at", 5, "from_alignment.checked_at"),
			},
		},
		"GET /v1/send-byo/accounts/{id} | error not_found": {
			Do: call(func(ctx context.Context, c *m3.Client, t testing.TB) (any, error) { return c.GetAccount(ctx, acct1) }), Kind: modclient.NotFound, Code: "not_found", Mutations: []sendapitest.Mutation{brokenEnvelope()},
		},
		"POST /v1/send-byo/accounts | created": {
			Do: call(func(ctx context.Context, c *m3.Client, t testing.TB) (any, error) {
				return c.CreateAccount(ctx, newAcct(t, 1))
			}),
			Check: func(t testing.TB, v any, err error) {
				a := v.(m3.Account)
				accountMatches(t, a, created)
				same(t, "id", a.ID, acct1)
				same(t, "user_id", a.UserID, userA)
				if a.Verification.State != m3.VerificationVerified || a.Verification.Code != m3.VerifyNone {
					t.Fatalf("the recording is not a create that verified: %+v", a.Verification)
				}
			},
			Mutations: []sendapitest.Mutation{
				del("id", "id"), set("user_id", other, "user_id"), del("verification", "verification"), set("verification.state", 5, "verification.state"), set("has_credential", "true", "has_credential"),
				set("created_at", "x", "created_at"),
			},
		},
		"POST /v1/send-byo/accounts | created failed": {
			Do: call(func(ctx context.Context, c *m3.Client, t testing.TB) (any, error) {
				return c.CreateAccount(ctx, newAcct(t, 2))
			}),
			Check: func(t testing.TB, v any, err error) {
				// a create whose credential check failed is still a created account, with the reason the module gave
				a := v.(m3.Account)
				accountMatches(t, a, createdFailed)
				if a.Verification.State != m3.VerificationFailed || a.Verification.Code != m3.VerifyAuthFailed || a.Verification.VerifiedAt != nil || a.Verification.VerifyAt == nil || a.Verification.SMTPResponse == nil {
					t.Fatalf("the recording is not a create whose credential check failed: %+v", a.Verification)
				}
			},
			Mutations: []sendapitest.Mutation{
				set("verification.verify_at", "x", "verification.verify_at"), set("from_alignment.checked_at", "x", "from_alignment.checked_at"), set("verification.code", 7, "verification.code"),
			},
		},
		"POST /v1/send-byo/accounts | error validation_failed": {
			Do: call(func(ctx context.Context, c *m3.Client, t testing.TB) (any, error) {
				return c.CreateAccount(ctx, newAcct(t, 3))
			}), Kind: modclient.Refused, Code: "validation_failed",
			Mutations: []sendapitest.Mutation{brokenEnvelope()},
			Check: func(t testing.TB, v any, err error) {
				if d := detailFields(err); !slices.Equal(d, docDetails(t, createErrValidation)) {
					t.Fatalf("details = %v, the recording says %v", d, docDetails(t, createErrValidation))
				}
			},
		},
		"POST /v1/send-byo/accounts | error duplicate": {
			Do: call(func(ctx context.Context, c *m3.Client, t testing.TB) (any, error) {
				return c.CreateAccount(ctx, newAcct(t, 4))
			}), Kind: modclient.Refused, Code: "conflict",
			Mutations: []sendapitest.Mutation{brokenEnvelope()},
			Check: func(t testing.TB, v any, err error) {
				// the module names smtp_username in a duplicate's details and in nothing else: the mapping follows what the RECORDING says, and
				// TestACreateConflictIsADuplicateOnlyWhenTheModuleNamesTheUsername holds the other side
				createConflictMapsAs(t, err, dupBody)
			},
		},
		"POST /v1/send-byo/accounts | error conflict": {
			Do: call(func(ctx context.Context, c *m3.Client, t testing.TB) (any, error) {
				return c.CreateAccount(ctx, newAcct(t, 5))
			}), Kind: modclient.Refused, Code: "conflict",
			Mutations: []sendapitest.Mutation{brokenEnvelope()},
			Check: func(t testing.TB, v any, err error) {
				createConflictMapsAs(t, err, createErrConflict)
			},
		},
		"POST /v1/send-byo/accounts | error unprocessable": {
			Do: call(func(ctx context.Context, c *m3.Client, t testing.TB) (any, error) {
				return c.CreateAccount(ctx, newAcct(t, 6))
			}), Kind: modclient.Refused, Code: "unprocessable",
			Mutations: []sendapitest.Mutation{brokenEnvelope()},
			Check: func(t testing.TB, v any, err error) {
				if d := detailFields(err); !slices.Equal(d, []string{"acknowledge_alignment_warning/required"}) || errors.Is(err, sendapi.ErrConflictUnexplained) {
					t.Fatalf("an unacknowledged warning: %v %v", d, err)
				}
			},
		},
		"POST /v1/send-byo/accounts/{id}/test | ok": {
			Do: call(func(ctx context.Context, c *m3.Client, t testing.TB) (any, error) {
				return c.TestAccount(ctx, acct1, false)
			}),
			Check: func(t testing.TB, v any, err error) {
				r := v.(m3.TestResult)
				testMatches(t, r, testOK)
				if r.Verification.State != m3.VerificationVerified || r.Verification.Code != m3.VerifyNone || r.Alignment.NotStored {
					t.Fatalf("the recording is not a click that verified: %+v", r)
				}
			},
			Mutations: []sendapitest.Mutation{
				del("verification", "verification"), del("verification.state", "verification.state"), set("from_alignment.state", 5, "from_alignment.state"), del("from_alignment", "from_alignment"),
				set("verification.smtp_response", 5, "verification.smtp_response"),
			},
		},
		"POST /v1/send-byo/accounts/{id}/test | failed": {
			Do: call(func(ctx context.Context, c *m3.Client, t testing.TB) (any, error) {
				return c.TestAccount(ctx, acct1, true)
			}),
			Check: func(t testing.TB, v any, err error) {
				r := v.(m3.TestResult)
				testMatches(t, r, testFailed)
				if r.Verification.State != m3.VerificationFailed || r.Verification.Code != m3.VerifyAuthFailed {
					t.Fatalf("the recording is not a click whose credential failed: %+v", r)
				}
			},
			Mutations: []sendapitest.Mutation{
				set("verification.code", 5, "verification.code"), set("verification.smtp_response", 5, "verification.smtp_response"), set("from_alignment.state", 5, "from_alignment.state"),
			},
		},
		"POST /v1/send-byo/accounts/{id}/test | unavailable": {
			Do: call(func(ctx context.Context, c *m3.Client, t testing.TB) (any, error) {
				return c.TestAccount(ctx, acct1, false)
			}),
			Check: func(t testing.TB, v any, err error) {
				r := v.(m3.TestResult)
				if r.Verification.State != m3.VerificationUnavailable || r.Verification.Code != m3.VerifyError || r.Alignment.State != m3.AlignmentIndeterminate || r.Verification.SMTPResponse != nil {
					t.Fatalf("result = %+v", r)
				}
			},
			Mutations: []sendapitest.Mutation{set("verification.state", 5, "verification.state"), set("from_alignment.code", 5, "from_alignment.code")},
		},
		"POST /v1/send-byo/accounts/{id}/test | error rate_limited": {
			Do: call(func(ctx context.Context, c *m3.Client, t testing.TB) (any, error) {
				return c.TestAccount(ctx, acct1, false)
			}), Kind: modclient.RateLimited, Code: "rate_limited",
			Mutations: []sendapitest.Mutation{brokenEnvelope()},
		},
		"POST /v1/send-byo/accounts/{id}/test | error conflict": {
			Do: call(func(ctx context.Context, c *m3.Client, t testing.TB) (any, error) {
				return c.TestAccount(ctx, acct1, false)
			}), Kind: modclient.Refused, Code: "conflict",
			Mutations: []sendapitest.Mutation{brokenEnvelope()},
			Check: func(t testing.TB, v any, err error) {
				if !errors.Is(err, m3.ErrActionUnavailable) || errors.Is(err, sendapi.ErrConflictUnexplained) {
					t.Fatalf("the test button's 409: %v", err)
				}
			},
		},
		"POST /v1/send-byo/accounts/{id}/test | error not_found": {
			Do: call(func(ctx context.Context, c *m3.Client, t testing.TB) (any, error) {
				return c.TestAccount(ctx, acct1, false)
			}), Kind: modclient.NotFound, Code: "not_found",
			Mutations: []sendapitest.Mutation{brokenEnvelope()},
		},
		"POST /v1/send-byo/accounts/bulk | ok": {
			Do: call(func(ctx context.Context, c *m3.Client, t testing.TB) (any, error) {
				return c.BulkAddAccounts(ctx, batch(t, bulkN))
			}),
			Check: func(t testing.TB, v any, err error) {
				// a mixed batch (bulkMatches fails on one that is not): every row, created or rejected, is held to the recording
				r := v.(m3.BulkResult)
				bulkMatches(t, r, bulkOK)
				var dup, invalid bool
				for _, x := range r.Rows {
					dup = dup || x.Reason == m3.RejectDuplicate
					invalid = invalid || x.Reason == m3.RejectValidation
				}
				if !dup || !invalid {
					t.Fatalf("the recording holds no duplicate row or no validation row: %+v", r.Rows)
				}
			},
			Mutations: []sendapitest.Mutation{
				del("results", "results"), del("results[0].status", "results[0].status"), set("results[1].status", "maybe", "results[1].status"), del(fmt.Sprintf("results[%d]", bulkN-1), "results"), set("results[2].row", 9, "results[2].row"),
				set("results[0].account_id", "x", "results[0].account_id"), set("results[0].verification", 5, "results[0].verification"), del("request_id", "request_id"),
				set("results[1].details[0].field", 5, "results[1].details[0].field"), del("results[0].row", "results[0].row"), set("results[2].status", "created", "results[2].account_id"),
			},
		},
		"POST /v1/send-byo/accounts/bulk | error batch_too_large": {
			Do: call(func(ctx context.Context, c *m3.Client, t testing.TB) (any, error) {
				return c.BulkAddAccounts(ctx, batch(t, 2))
			}), Kind: modclient.Refused, Code: "unprocessable",
			Mutations: []sendapitest.Mutation{brokenEnvelope()},
		},
		"POST /v1/send-byo/accounts/bulk | error conflict": {
			Do: call(func(ctx context.Context, c *m3.Client, t testing.TB) (any, error) {
				return c.BulkAddAccounts(ctx, batch(t, 2))
			}), Kind: modclient.Refused, Code: "conflict",
			Mutations: []sendapitest.Mutation{brokenEnvelope()},
			Check: func(t testing.TB, v any, err error) {
				if !errors.Is(err, m3.ErrActionUnavailable) {
					t.Fatalf("the bulk add's 409: %v", err)
				}
			},
		},
		"POST /v1/send-byo/accounts/bulk | error validation_failed": {
			Do: call(func(ctx context.Context, c *m3.Client, t testing.TB) (any, error) {
				return c.BulkAddAccounts(ctx, batch(t, 2))
			}), Kind: modclient.Refused, Code: "validation_failed",
			Mutations: []sendapitest.Mutation{brokenEnvelope()},
		},
		"GET /v1/send-byo/accounts/{id}/events | page": {
			Do: call(func(ctx context.Context, c *m3.Client, t testing.TB) (any, error) {
				return c.AccountEvents(ctx, acct1, "")
			}),
			Check: func(t testing.TB, v any, err error) {
				p := v.(m3.EventPage)
				if len(p.Items) != 2 || p.NextCursor == "" || p.Items[0].From != nil || p.Items[0].To == nil || p.Items[0].Actor != "user" || p.Items[0].Action != "created" || p.Items[0].AccountID != acct1 ||
					p.Items[1].Reason == nil || *p.Items[1].Reason != "daily limit reached" || p.Items[1].To == nil || p.Items[1].To.PauseReason != "daily_limit" || !p.Items[1].To.Paused || p.Items[1].RequestID != "" {
					t.Fatalf("page = %+v", p)
				}
			},
			Mutations: []sendapitest.Mutation{
				del("items[0].actor", "items[0].actor"), set("items[1].user_id", other, "items[1].user_id"), set("items[0].created_at", "x", "items[0].created_at"), set("items[1].to_flags.paused", "x", "items[1].to_flags.paused"),
				set("next_cursor", 5, "next_cursor"), del("items[1].from_flags", "items[1].from_flags"), set("items[0].id", "", "items[0].id"),
			},
		},
		"GET /v1/send-byo/accounts/{id}/events | empty": {
			Do: call(func(ctx context.Context, c *m3.Client, t testing.TB) (any, error) {
				return c.AccountEvents(ctx, acct1, "")
			}),
			Check: func(t testing.TB, v any, err error) {
				if p := v.(m3.EventPage); len(p.Items) != 0 || p.NextCursor != "" {
					t.Fatalf("page = %+v", p)
				}
			},
			Mutations: []sendapitest.Mutation{set("next_cursor", "abc", "next_cursor")},
		},
	}
}

// Every response of module 3 that the adapter relies on is served from its recorded file by an httptest server and decoded through the adapter's real code (the client, the walk, the
// accounts, the describe, the feasibility); each recorded body conforms to the Shape its contract names; and every mutation (a required key removed, a key's type changed, a state outside the
// vocabulary, an unreadable timestamp, another tenant's account, a contradictory verdict) is refused with the key it names. The unmutated body is the control of the mutations, through the
// same server and call.
func TestContractsOfModule3(t *testing.T) {
	sendapitest.Suite{Dir: fixtures, Contracts: m3.Contracts(), Cases: cases(t)}.Run(t)
}

// No rot: the files of testdata/contract and the contracts name each other exactly once, and the contracts are the shared ones plus module 3's own. The controls: a stray file, a missing file
// and a doubly named file are each caught.
func TestContractFixturesDoNotRot(t *testing.T) {
	cs := m3.Contracts()
	if problems := sendapitest.CheckNoRot(fixtures, cs); len(problems) != 0 {
		t.Fatalf("the fixtures have rotted: %v", problems)
	}
	shared := sendapi.Contracts(m3.Profile())
	if len(shared) == 0 || len(cs) <= len(shared) {
		t.Fatalf("%d contracts of which %d shared: the subject is empty", len(cs), len(shared))
	}
	entries, err := os.ReadDir(fixtures)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(cs) {
		t.Fatalf("%d files and %d contracts", len(entries), len(cs))
	}
	copyDir := func(t *testing.T) string {
		dir := t.TempDir()
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
	stray := copyDir(t)
	if err := os.WriteFile(filepath.Join(stray, "stray.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if p := sendapitest.CheckNoRot(stray, cs); len(p) != 1 || !anyHas(p, "stray.json is named by no contract") {
		t.Fatalf("a stray file: %v", p)
	}
	missing := copyDir(t)
	if err := os.Remove(filepath.Join(missing, "accounts.bulk.ok.json")); err != nil {
		t.Fatal(err)
	}
	if p := sendapitest.CheckNoRot(missing, cs); len(p) != 1 || !anyHas(p, "names the missing file accounts.bulk.ok.json") {
		t.Fatalf("a missing file: %v", p)
	}
	twice := append([]modclient.RouteContract(nil), cs...)
	twice[len(twice)-1].Fixture = twice[len(twice)-2].Fixture
	if p := sendapitest.CheckNoRot(fixtures, twice); !anyHas(p, "is named by 2 contracts") {
		t.Fatalf("a file named twice: %v", p)
	}
}

// The contracts name module 3 and nothing else, carry the shared ones unchanged, and the account routes are the ones the adapter has a method for.
func TestContractsNameModule3AndItsShapes(t *testing.T) {
	cs := m3.Contracts()
	byKey := map[string]modclient.RouteContract{}
	for _, c := range cs {
		if c.Module != modclient.M3 {
			t.Fatalf("%s / %s names module %s", c.Route, c.Case, c.Module)
		}
		byKey[sendapitest.Key(c)] = c
	}
	for _, c := range sendapi.Contracts(m3.Profile()) {
		got, ok := byKey[sendapitest.Key(c)]
		if !ok || !reflect.DeepEqual(got, c) {
			t.Fatalf("the shared contract %q is not carried unchanged", sendapitest.Key(c))
		}
	}
	routes := map[string]bool{}
	for _, c := range cs {
		routes[c.Route] = true
	}
	for _, r := range []string{m3.RouteQuote, m3.RouteAccountsList, m3.RouteAccountsGet, m3.RouteAccountsCreate, m3.RouteAccountsTest, m3.RouteAccountsBulk, m3.RouteAccountsEvents, sendapi.RouteRun, sendapi.RouteReport, sendapi.RouteDescribe} {
		if !routes[r] {
			t.Fatalf("no contract for %s", r)
		}
	}
	// the error contracts all name the nine codes, the same set the profile does
	for _, c := range cs {
		if strings.Contains(c.Case, "error") {
			if !slices.Equal(c.Vocab["error_codes"], m3.Profile().ErrorCodes) {
				t.Fatalf("%s / %s: error vocabulary %v", c.Route, c.Case, c.Vocab)
			}
			if c.Route == sendapi.RouteRun && c.Case == "error conflict" { // the one error whose details carry more than field and issue (the cause, and for a cap the numbers)
				if reflect.DeepEqual(c.Shape, sendapi.ErrorShape()) || !slices.Equal(c.Vocab["run_conflict_cause"], causeWords()) {
					t.Fatalf("the run conflict contract is the shared error shape, or does not carry the closed causes: %v", c.Vocab)
				}
				continue
			}
			if !reflect.DeepEqual(c.Shape, sendapi.ErrorShape()) {
				t.Fatalf("%s / %s: not the shared error shape", c.Route, c.Case)
			}
		}
	}
	// the Vocab of one entry is a copy: editing one edits no other
	a, b := m3.Contracts(), m3.Contracts()
	for i := range a {
		for name, v := range a[i].Vocab {
			if len(v) > 0 {
				v[0] = "tampered"
				if b[i].Vocab[name][0] == "tampered" || m3.Contracts()[i].Vocab[name][0] == "tampered" {
					t.Fatal("two contracts share a vocabulary slice")
				}
			}
		}
	}
}

// ---- the closed sets are the switches ---------------------------------------------------------------------------------------

// switchCases reads the string literals of the case clauses of each of the named functions of vocab.go from the source: the adapter's own switches, as written. The empty string (a switch's
// "no value" case) is not a member of a set.
func switchCases(t testing.TB, file string, funcs ...string) map[string][]string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || !slices.Contains(funcs, fn.Name.Name) {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if cc, ok := n.(*ast.CaseClause); ok {
				for _, e := range cc.List {
					if bl, ok := e.(*ast.BasicLit); ok && bl.Kind == token.STRING {
						if s, _ := strconv.Unquote(bl.Value); s != "" {
							out[fn.Name.Name] = append(out[fn.Name.Name], s)
						}
					}
				}
			}
			return true
		})
	}
	for k := range out {
		sort.Strings(out[k])
	}
	return out
}

// setDiff compares what a switch handles with a closed set: a value the switch handles that the set does not list, and a value the set lists that the switch does not handle, each named.
func setDiff(handled, want []string) []string {
	var problems []string
	for _, h := range handled {
		if !slices.Contains(want, h) {
			problems = append(problems, "the switch handles "+h+", which the vocabulary does not list")
		}
	}
	for _, w := range want {
		if !slices.Contains(handled, w) {
			problems = append(problems, "the vocabulary lists "+w+", which the switch does not handle")
		}
	}
	return problems
}

// The closed sets of the contracts' Vocab are exactly the switches that map from them (none handled that the set does not list, none listed that is not handled), every set a contract names
// is covered by a switch (or is one of the shared body's, which its own test holds), and one name is one set across every contract that carries it. The controls run the same comparison on a
// switch with an extra case and one with a case missing.
func TestVocabIsTheSwitchesAndTheSwitchesAreTheVocab(t *testing.T) {
	bySwitch := map[string]string{
		"verificationState": "credential_verification_state", "testVerificationState": "test_verification_state", "alignmentState": "from_verification_state",
		"testAlignmentState": "test_alignment_state", "verifyCode": "verify_code", "rejectReason": "bulk_reject_code", "quoteReason": "quote_reason_code", "pauseReason": "pause_reason",
		"quarantineReason": "quarantine_reason", "bulkStatus": "bulk_add_row_status", "alignmentCode": "alignment_code", "alignmentError": "alignment_error",
	}
	var fns []string
	for fn := range bySwitch {
		fns = append(fns, fn)
	}
	handled := switchCases(t, "vocab.go", fns...)
	// the vocabularies the contracts name, one set per name
	vocab := map[string][]string{}
	for _, c := range m3.Contracts() {
		for name, v := range c.Vocab {
			if prev, ok := vocab[name]; ok && !slices.Equal(prev, v) {
				t.Fatalf("the vocabulary %s is two different sets: %v and %v", name, prev, v)
			}
			vocab[name] = v
		}
	}
	sharedNames := []string{"job_state", "error_class", "error_codes", "item_reason", "run_conflict_cause"}
	// the sets that no contract can carry because no fixture holds a string at their position (a quarantine reason is absent from every healthy account, a bulk row's rejection reasons
	// share the one position of its code with the verification codes): the first is the module's published set, the second the share of the union that is not a verification code
	noPosition := []string{"quarantine_reason", "bulk_reject_code", "alignment_error"}
	covered := map[string]bool{}
	for fn, name := range bySwitch {
		covered[name] = true
		want := vocab[name]
		if len(want) == 0 {
			if !slices.Contains(noPosition, name) {
				t.Fatalf("no contract names the vocabulary %s of the switch %s: the subject is empty", name, fn)
			}
			want = vocabSet(t, name)
		}
		if len(want) == 0 {
			t.Fatalf("the vocabulary %s of the switch %s is empty", name, fn)
		}
		got := handled[fn]
		if len(got) == 0 {
			t.Fatalf("the instrument matched nothing: no case literal found in %s", fn)
		}
		if p := setDiff(got, want); len(p) != 0 {
			t.Fatalf("%s and the vocabulary %s disagree: %v", fn, name, p)
		}
		// controls
		if p := setDiff(append(slices.Clone(got), "brand_new"), want); len(p) != 1 || !strings.Contains(p[0], "the switch handles brand_new") {
			t.Fatalf("an extra case was not caught for %s: %v", fn, p)
		}
		if p := setDiff(got[1:], want); len(p) != 1 || !strings.Contains(p[0], "the switch does not handle") {
			t.Fatalf("a missing case was not caught for %s: %v", fn, p)
		}
	}
	// a bulk row's code as a whole is the union of two switches, which share no word
	union := append(slices.Clone(handled["rejectReason"]), handled["verifyCode"]...)
	covered["bulk_row_code"] = true
	if p := setDiff(union, vocab["bulk_row_code"]); len(p) != 0 || len(handled["rejectReason"]) == 0 || len(handled["verifyCode"]) == 0 {
		t.Fatalf("a bulk row's code and the two switches that map it disagree: %v", p)
	}
	for _, w := range handled["rejectReason"] {
		if slices.Contains(handled["verifyCode"], w) {
			t.Fatalf("the word %s is both a rejection reason and a verification code: the one position of a bulk row's code would be ambiguous", w)
		}
	}
	if p := setDiff(append(slices.Clone(union), "brand_new"), vocab["bulk_row_code"]); len(p) != 1 || !strings.Contains(p[0], "the switch handles brand_new") {
		t.Fatalf("an extra case was not caught for a bulk row's code: %v", p)
	}
	if p := setDiff(union[1:], vocab["bulk_row_code"]); len(p) != 1 || !strings.Contains(p[0], "the switch does not handle") {
		t.Fatalf("a missing case was not caught for a bulk row's code: %v", p)
	}
	// the sets the module publishes in describe are held to the module's own list too (the recorded describe, an independent source): every switch the module has a set for handles exactly it
	published := map[string]string{
		"verificationState": "credential_verification_state", "alignmentState": "from_verification_state", "quoteReason": "quote_reason_code", "pauseReason": "pause_reason",
		"quarantineReason": "quarantine_reason", "bulkStatus": "bulk_add_row_status",
	}
	for fn, name := range published {
		want := vocabSet(t, "describe:"+name)
		if p := setDiff(handled[fn], want); len(p) != 0 {
			t.Fatalf("%s and the module's published %s disagree: %v", fn, name, p)
		}
		if p := setDiff(append(slices.Clone(handled[fn]), "brand_new"), want); len(p) != 1 {
			t.Fatalf("an extra case was not caught against the published %s: %v", name, p)
		}
	}
	for name := range vocab {
		if !covered[name] && !slices.Contains(sharedNames, name) {
			t.Fatalf("the vocabulary %s is named by a contract and handled by no switch of this package", name)
		}
	}
	// the shared sets are the profile's own
	p := m3.Profile()
	for name, want := range map[string][]string{"job_state": p.JobStates, "error_class": p.ErrorClasses, "error_codes": p.ErrorCodes} {
		if got, ok := vocab[name]; !ok || !slices.Equal(got, want) {
			t.Fatalf("%s is %v on the contracts, the profile says %v", name, got, want)
		}
	}
}

func reflectDump(v any) string {
	var b strings.Builder
	var walk func(rv reflect.Value)
	walk = func(rv reflect.Value) {
		switch rv.Kind() {
		case reflect.String:
			b.WriteString(rv.String() + "|")
		case reflect.Pointer, reflect.Interface:
			if !rv.IsNil() {
				walk(rv.Elem())
			}
		case reflect.Struct:
			for i := 0; i < rv.NumField(); i++ {
				if rv.Type().Field(i).IsExported() {
					walk(rv.Field(i))
				}
			}
		case reflect.Slice:
			for i := 0; i < rv.Len(); i++ {
				walk(rv.Index(i))
			}
		}
	}
	walk(reflect.ValueOf(v))
	return b.String()
}
