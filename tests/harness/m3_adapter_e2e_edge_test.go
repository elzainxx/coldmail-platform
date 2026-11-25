//go:build harness

package harness

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/adapters/m3"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// Phase 3, chunk B6c for module 3: the contract drift gate's edge cases. The end-to-end tests of m3_adapter_e2e_test.go reach the answers a healthy flow gets; this test makes module 3 give the other
// answers its contracts (internal/adapters/m3/contracts.go) declare and the first record run did not reach, each from a client whose options carry the drift session's observer (m3Drift.Observer(), put
// into every client m3aEnv builds), so that every real answer is compared with the recorded fixture of its contract by TestM3Adapter_ZContractDrift, which runs after this file (zz_).
//
// It is ONE test with one tenant (X, "edge") and independent steps: a step that finds another answer than the one it asked for says so (t.Errorf, naming the status and the code the module gave) and the
// next step still runs, so one wrong guess of the research costs one line of the report and not the run. The steps, the contract each reaches and how (a paired control follows each: the same call with
// correct input, or the same state without the dial, succeeds):
//
//	describe             GET /v1/describe ok                              Adapter.Describe
//	quote                POST /v1/quote feasible                          Feasibility(100) with module 3's own quote_long_duration_warn_sec lowered to 60 s: a feasible quote WITH its long_duration warning (restored at once)
//	accounts, create     POST /v1/send-byo/accounts validation_failed     CreateAccount with a from address that is not an address (the adapter checks only host and secret): 400; the same account with a good address is created
//	                     ... conflict (duplicate, with details)           the same account again: 409 naming smtp_username
//	                     ... conflict (cap)                               account_count_cap_per_user lowered to the number of accounts: 409 with no details; restored, the same create succeeds
//	accounts, list       GET /v1/send-byo/accounts cursor_malformed       ListAccounts with a cursor that is not base64: 400
//	                     ... validation_failed                            the same route with no user_id (a raw call through the observed module client): 400
//	                     ... page, last page                              limit=2 over the tenant's three accounts: a page with a next cursor, then the last page
//	accounts, events     GET /v1/send-byo/accounts/{id}/events page       limit=1 over an account with two events (created, verified): a page with a next cursor, then the next
//	accounts, test       POST /v1/send-byo/accounts/{id}/test rate_limited  the test button clicked until module 3's own per-account limit says 429 (the create counts as one verification)
//	accounts, bulk       POST /v1/send-byo/accounts/bulk validation_failed  an empty batch (raw): 400
//	                     ... batch_too_large                              bulk_add_max_rows lowered to 1, two rows through BulkAddAccounts: 422 and nothing created; restored, the same two rows are created
//	run                  POST /v1/run unauthenticated                     an adapter built over a random service token: 401
//	                     ... validation_failed                            an empty body (raw): 400
//	                     ... paused                                       dispatch_mode set to refuse_new for the one call and restored at once: 503, no job created; restored, the same Run is accepted
//	report, held queue   GET /v1/report job queued                        five items, ONE lane held for 60 s after every push, the sink answering 451: the item module 3 tried is queued with attempt_count >= 1, transport_4xx, a 451 text
//	                                                                      and (the first record run showed) the account of the failed attempt still attached, asserted only as consistent; the four it did not try are queued with attempt_count 0 and nothing else
//	                     ... campaign page, last page                     the five at page size 2: pages of 2, 2 and 1 (the last has no next cursor and, being a never-tried item, no account: the shape of the last-page contract)
//	                     ... campaign cursor_malformed                    a cursor that is not base64: 400; and, since Phase 4 A1, a base64 cursor that is not a timestamp and an id: the same 400 (it was a 500, and had a contract of its own, cursor_forged, removed)
//	events, system row   GET /v1/send-byo/accounts/{id}/events page       a tenant of its own (ev) with one account whose send the sink refuses AUTH for: an auth_failure, at autoquarantine_mode=shadow module 3 writes an
//	                                                                      "updated" event with flags before and after and a reason: the events listed through the adapter and, at limit=1, page by page through the raw client
//
// WHAT IS ALLOWLISTED (zz_m3_adapter_contract_drift_test.go, with the reason): run internal_error (a failing statement inside module 3), accounts test unavailable (module 3's verifier failing or missing) and
// the events case `page` (a page with a cursor and a pause event, which no page of this harness is). The From-gate's 422 (accounts create unprocessable) left the list when the adapter tests turned the gate on
// (Phase 3 close-out D2, TestM3Adapter_FromGatePassWarnBlock), and the events of an enforced quarantine are the case `last page` (D3, TestM3Adapter_AccountEventsOfAnEnforcedHealthAction).
//
// THE LEFT-OVERS. X (and then ev) is suspended through module 3's operator route when its steps are done, so the queued items (the sink answered 451 or refused AUTH, and module 3 retries) are never claimed again and nothing of them is sent after the
// sink is reset; X has no entitlement row, so no platform loop looks at it. The dials this test lowers (quote_long_duration_warn_sec, account_count_cap_per_user, bulk_add_max_rows, dispatch_mode) are put back
// the moment each step is done, and again when the test ends if a step died in between.
func TestM3Adapter_ContractEdgeCases(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), m3aBudget)
	t.Cleanup(cancel)
	e := newM3aEnv(t, ctx)
	e.prepare(t, true, true)

	x := e.seedTenant(t, "edge", []string{"defer", "defer", "defer", "defer", "defer"})
	step := func(name string, fn func()) {
		t.Helper()
		start := time.Now()
		fn()
		t.Logf("NOTE m3a edge step %s done in %s", name, time.Since(start).Round(time.Millisecond))
	}
	list := func(q url.Values) (n int, next string) {
		t.Helper()
		resp, err := e.rawDo(m3aRawCall{label: "m3.edge.accounts.list", method: http.MethodGet, path: "/v1/send-byo/accounts", query: q})
		if err != nil {
			t.Errorf("GET %s %v: %s", "/v1/send-byo/accounts", q, e.raw.scrub(err.Error()))
			return 0, ""
		}
		return m3aPageOf(t, resp.Body)
	}

	// ---- describe ----
	step("describe", func() {
		d, err := e.ad.Describe(ctx)
		if err != nil {
			t.Errorf("Describe through the adapter failed (%s): %s", modclient.KindOf(err), e.raw.scrub(err.Error()))
			return
		}
		t.Logf("NOTE m3a edge describe: module %s api %s, %d capabilities, %d vocabularies, %d error codes", d.Module, d.APIVersion, len(d.Capabilities), len(d.Vocabularies), len(d.ErrorCodes))
	})

	// ---- the account of X, the quote ----
	e.createVerified(t, x, "1", e.sinkIP)
	step("quote feasible with a warning", func() {
		e.withDial(t, "quote_long_duration_warn_sec", "60", func() {
			fz, err := x.C.Feasibility(ctx, 100)
			if err != nil {
				t.Errorf("Feasibility(100) through the adapter failed (%s): %s", modclient.KindOf(err), e.raw.scrub(err.Error()))
				return
			}
			if !fz.Feasible || len(fz.Reasons) != 0 || len(fz.Warnings) != 1 || fz.Warnings[0].Code != "long_duration" || fz.Warnings[0].ThresholdSec != 60 || fz.Warnings[0].EstimatedDurationSec < 60 {
				t.Errorf("a quote of 100 recipients with the warning threshold at 60 s is feasible=%v reasons=%+v warnings=%+v, want feasible with one long_duration warning over a 60 s threshold", fz.Feasible, fz.Reasons, fz.Warnings)
			}
			proj := "none"
			if p := fz.Capacity.EstimatedDurationSec; p != nil {
				proj = fmt.Sprintf("%d s", *p)
			}
			t.Logf("NOTE m3a edge quote: 100 recipients, threshold 60 s: feasible %v, projection %s, warnings %+v", fz.Feasible, proj, fz.Warnings)
		})
		if fz, err := x.C.Feasibility(ctx, 100); err != nil || !fz.Feasible || len(fz.Warnings) != 0 {
			t.Errorf("control: the same quote with the dial restored answered %+v (error %v), want feasible with no warning", fz, err)
		}
	})

	// ---- accounts: create ----
	step("accounts create", func() {
		good := e.newAccount("dup", x, e.sinkIP)
		bad := good
		bad.FromAddress = "not-an-address"
		_, err := x.C.CreateAccount(ctx, bad)
		if me := m3aWantAnswer(t, "CreateAccount with a from address that is not an address", err, modclient.Refused, 400, "validation_failed"); me != nil {
			if m3.MaybeApplied(err) {
				t.Errorf("a refused create says it may have been applied")
			}
			t.Logf("NOTE m3a edge create validation_failed: HTTP %d code %s details %v request %s", me.Status, me.Code, me.Details, me.RequestID)
		}
		first, err := x.C.CreateAccount(ctx, good) // the control: the same account with a good from address
		if err != nil || first.ID == "" {
			t.Errorf("control: the same account with a good from address answered %+v (error %v), want it created", first, err)
		} else {
			x.Accounts = append(x.Accounts, first)
		}
		_, err = x.C.CreateAccount(ctx, good)
		if me := m3aWantAnswer(t, "CreateAccount of an account that exists", err, modclient.Refused, 409, "conflict"); me != nil {
			if !errors.Is(err, m3.ErrAccountDuplicate) || len(me.Details) != 1 || me.Details[0].Field != "smtp_username" || me.Details[0].Issue != "conflict" {
				t.Errorf("the duplicate create answered %v (details %v), want ErrAccountDuplicate with the detail {smtp_username, conflict}", err, me.Details)
			}
			t.Logf("NOTE m3a edge create duplicate: HTTP %d code %s details %v", me.Status, me.Code, me.Details)
		}
	})
	rl := e.createVerified(t, x, "rl", e.sinkIP)

	// ---- accounts: list ----
	step("accounts list", func() {
		_, err := x.C.ListAccounts(ctx, "!!!not base64!!!")
		if me := m3aWantAnswer(t, "ListAccounts with a cursor that is not base64", err, modclient.Refused, 400, "validation_failed"); me != nil {
			if len(me.Details) != 1 || me.Details[0].Field != "cursor" || me.Details[0].Issue != "malformed" {
				t.Errorf("the malformed cursor answered details %v, want {cursor, malformed}", me.Details)
			}
			t.Logf("NOTE m3a edge list cursor_malformed: HTTP %d code %s details %v", me.Status, me.Code, me.Details)
		}
		_, err = e.rawDo(m3aRawCall{label: "m3.edge.accounts.list", method: http.MethodGet, path: "/v1/send-byo/accounts"}) // no user_id
		if me := m3aWantAnswer(t, "GET /v1/send-byo/accounts with no user_id", err, modclient.Refused, 400, "validation_failed"); me != nil {
			if len(me.Details) != 1 || me.Details[0].Field != "user_id" || me.Details[0].Issue != "required" {
				t.Errorf("the list with no user_id answered details %v, want {user_id, required}", me.Details)
			}
			t.Logf("NOTE m3a edge list validation_failed: HTTP %d code %s details %v", me.Status, me.Code, me.Details)
		}
		all, err := x.C.AllAccounts(ctx) // the control: the same list with the tenant is the tenant's accounts
		if err != nil || len(all) != 3 {
			t.Errorf("control: the tenant's list answered %d account(s) (error %v), want its three", len(all), err)
		}
		n1, next := list(url.Values{"user_id": {x.UserID}, "limit": {"2"}})
		if n1 != 2 || next == "" {
			t.Errorf("limit=2 over three accounts answered %d account(s) and next cursor %q, want a page of 2 with a next cursor", n1, next)
		} else {
			n2, next2 := list(url.Values{"user_id": {x.UserID}, "limit": {"2"}, "cursor": {next}})
			if n2 != 1 || next2 != "" {
				t.Errorf("the page after the first answered %d account(s) and next cursor %q, want the last page: one account and no cursor", n2, next2)
			}
			t.Logf("NOTE m3a edge list pages: limit=2 over %d accounts: a page of %d with a next cursor, then a last page of %d with none", len(all), n1, n2)
		}
	})

	// ---- accounts: events ----
	step("accounts events", func() {
		path := "/v1/send-byo/accounts/" + url.PathEscape(rl.ID) + "/events"
		resp, err := e.rawDo(m3aRawCall{label: "m3.edge.accounts.events", method: http.MethodGet, path: path, query: url.Values{"user_id": {x.UserID}, "limit": {"1"}}})
		if err != nil {
			t.Errorf("GET %s with limit=1 failed: %s", "events", e.raw.scrub(err.Error()))
			return
		}
		n, next := m3aPageOf(t, resp.Body)
		if n != 1 || next == "" {
			t.Errorf("limit=1 over the events of an account that was created and verified answered %d event(s) and next cursor %q, want one event and a next cursor", n, next)
			return
		}
		resp2, err := e.rawDo(m3aRawCall{label: "m3.edge.accounts.events", method: http.MethodGet, path: path, query: url.Values{"user_id": {x.UserID}, "limit": {"1"}, "cursor": {next}}})
		if err != nil {
			t.Errorf("the second page of the events failed: %s", e.raw.scrub(err.Error()))
			return
		}
		n2, next2 := m3aPageOf(t, resp2.Body)
		evs, err := x.C.AccountEvents(ctx, rl.ID, "")
		if err != nil || len(evs.Items) < 2 || n2 != 1 {
			t.Errorf("the second event page holds %d event(s); the adapter's own read answered %d event(s) (error %v), want one and at least two", n2, len(evs.Items), err)
		}
		t.Logf("NOTE m3a edge events: limit=1 over %d event(s): a page of %d with a next cursor, then a page of %d (next cursor %q)", len(evs.Items), n, n2, next2)
	})

	// ---- accounts: the test button's own limit ----
	step("accounts test rate_limited", func() {
		limit := e.dialInt(t, "account_test_rate_limit_per_day")
		ok, limited := 0, false
		for i := 1; i <= int(limit)+2; i++ {
			tr, err := x.C.TestAccount(ctx, rl.ID, false)
			if err == nil {
				if tr.Verification.State != m3.VerificationVerified {
					t.Errorf("click %d of the test button answered verification %q, want verified (the sink accepts)", i, tr.Verification.State)
				}
				ok++
				continue
			}
			if me := m3aWantAnswer(t, fmt.Sprintf("click %d of the test button", i), err, modclient.RateLimited, 429, "rate_limited"); me != nil {
				limited = true
				if m3.MaybeApplied(err) {
					t.Errorf("a refused test button click says it may have been applied")
				}
			}
			break
		}
		if !limited || ok < 1 || int64(ok) > limit {
			t.Errorf("the test button of one account answered %d click(s) before its limit of %d per day said 429 (limited %v): want the earlier clicks accepted (the create is one of the %d) and then the module's 429", ok, limit, limited, limit)
		}
		t.Logf("NOTE m3a edge test rate_limited: account_test_rate_limit_per_day %d; %d click(s) accepted after the create, then HTTP 429 rate_limited (limited %v)", limit, ok, limited)
	})

	// ---- accounts: bulk ----
	step("accounts bulk", func() {
		_, err := e.rawDo(m3aRawCall{label: "m3.edge.accounts.bulk", method: http.MethodPost, path: "/v1/send-byo/accounts/bulk", body: map[string]any{"user_id": x.UserID, "accounts": []any{}}})
		if me := m3aWantAnswer(t, "a bulk add of no account", err, modclient.Refused, 400, "validation_failed"); me != nil {
			if len(me.Details) != 1 || me.Details[0].Field != "accounts" || me.Details[0].Issue != "required" {
				t.Errorf("the empty batch answered details %v, want {accounts, required}", me.Details)
			}
			t.Logf("NOTE m3a edge bulk validation_failed: HTTP %d code %s details %v", me.Status, me.Code, me.Details)
		}
		rows := []m3.NewAccount{e.newAccount("b1", x, e.sinkIP), e.newAccount("b2", x, e.sinkIP)}
		before, _ := x.C.AllAccounts(ctx)
		e.withDial(t, "bulk_add_max_rows", "1", func() {
			_, err := x.C.BulkAddAccounts(ctx, rows)
			if me := m3aWantAnswer(t, "a bulk add of two rows with bulk_add_max_rows at 1", err, modclient.Refused, 422, "unprocessable"); me != nil {
				if len(me.Details) != 1 || me.Details[0].Field != "accounts" || me.Details[0].Issue != "out_of_range" || m3.MaybeApplied(err) {
					t.Errorf("the batch over the limit answered details %v (may have been applied %v), want {accounts, out_of_range} and nothing applied", me.Details, m3.MaybeApplied(err))
				}
				t.Logf("NOTE m3a edge bulk batch_too_large: HTTP %d code %s details %v", me.Status, me.Code, me.Details)
			}
		})
		mid, _ := x.C.AllAccounts(ctx)
		if len(mid) != len(before) {
			t.Errorf("module 3 holds %d account(s) after the refused batch, %d before: a batch that is too large must be refused whole", len(mid), len(before))
		}
		res, err := x.C.BulkAddAccounts(ctx, rows) // the control: the same rows with the dial restored are created
		if err != nil || len(res.Rows) != 2 || !res.Rows[0].Created || !res.Rows[1].Created {
			t.Errorf("control: the same two rows with the dial restored answered %+v (error %v), want both created", res, err)
		}
	})

	// ---- accounts: the cap ----
	step("accounts create conflict (cap)", func() {
		all, err := x.C.AllAccounts(ctx)
		if err != nil {
			t.Errorf("AllAccounts failed: %s", e.raw.scrub(err.Error()))
			return
		}
		e.withDial(t, "account_count_cap_per_user", fmt.Sprint(len(all)), func() {
			_, err := x.C.CreateAccount(ctx, e.newAccount("capped", x, e.sinkIP))
			if me := m3aWantAnswer(t, "CreateAccount at the account cap", err, modclient.Refused, 409, "conflict"); me != nil {
				if len(me.Details) != 0 || errors.Is(err, m3.ErrAccountDuplicate) || !errors.Is(err, sendapi.ErrConflictUnexplained) || m3.MaybeApplied(err) {
					t.Errorf("the create at the cap answered details %v (duplicate %v), want the 409 with no detail that the adapter calls ErrConflictUnexplained", me.Details, errors.Is(err, m3.ErrAccountDuplicate))
				}
				t.Logf("NOTE m3a edge create conflict: HTTP %d code %s details %v at a cap of %d accounts", me.Status, me.Code, me.Details, len(all))
			}
		})
		if a, err := x.C.CreateAccount(ctx, e.newAccount("capped", x, e.sinkIP)); err != nil || a.ID == "" { // the control: the same create with the dial restored
			t.Errorf("control: the same create with the cap restored answered %+v (error %v), want it created", a, err)
		}
	})

	// ---- run: unauthenticated, validation_failed, paused ----
	extra, extraCampaign := e.seedExtraItem(t, x, "paused")
	extraTn := &m3aTenant{Name: x.Name, UserID: x.UserID, CampaignID: extraCampaign, Prefix: x.Prefix, C: x.C, Accounts: x.Accounts, Items: []*m3aItem{{Index: 1, ID: extra.ID, Email: extra.Email, Kind: "ok"}}}
	step("run unauthenticated", func() {
		cfg := e.cfg
		cfg.M3ServiceToken = config.Secret(randomHex(t, 16)) // a token module 3 has never been given
		stranger, err := e.adapter(t, cfg, e.adm).For(x.UserID)
		if err != nil {
			t.Errorf("building the adapter over a random token failed: %s", e.raw.scrub(err.Error()))
			return
		}
		_, err = stranger.Run(ctx, extra.ID)
		if me := m3aWantAnswer(t, "Run with a token module 3 does not know", err, modclient.Refused, 401, "unauthenticated"); me != nil {
			t.Logf("NOTE m3a edge run unauthenticated: HTTP %d code %s request %s", me.Status, me.Code, me.RequestID)
		}
		if jobs := e.jobsOf(t, x.UserID); jobs[extra.ID].ID != "" {
			t.Errorf("module 3 holds a job for the item after a Run that carried an unknown token: a refused call must create nothing")
		}
	})
	step("run validation_failed", func() {
		_, err := e.rawDo(m3aRawCall{label: "m3.edge.run", method: http.MethodPost, path: "/v1/run", body: map[string]any{}, expect: []int{http.StatusOK, http.StatusAccepted}})
		if me := m3aWantAnswer(t, "POST /v1/run with an empty body", err, modclient.Refused, 400, "validation_failed"); me != nil {
			t.Logf("NOTE m3a edge run validation_failed: HTTP %d code %s details %v", me.Status, me.Code, me.Details)
		}
	})
	step("run paused", func() {
		e.withDial(t, "dispatch_mode", `"refuse_new"`, func() {
			_, err := x.C.Run(ctx, extra.ID)
			if me := m3aWantAnswer(t, "Run with dispatch_mode at refuse_new", err, modclient.Paused, 503, "paused"); me != nil {
				t.Logf("NOTE m3a edge run paused: HTTP %d code %s request %s (the module's Paused kind: wait, do not page)", me.Status, me.Code, me.RequestID)
			}
			if jobs := e.jobsOf(t, x.UserID); jobs[extra.ID].ID != "" {
				t.Errorf("module 3 holds a job for the item after a Run it refused as paused: a refused call must create nothing")
			}
		})
		res, err := x.C.Run(ctx, extra.ID) // the control: the same Run with the rung back at serving is accepted, and the item is sent
		if err != nil || !res.Created {
			t.Errorf("control: the same Run with dispatch_mode restored answered %+v (error %v), want a new job", res, err)
			return
		}
		for start := time.Now(); ; time.Sleep(500 * time.Millisecond) { // sent, so that the held campaign below is the only active one (module 3 allows one active campaign per user)
			rep, err := x.C.ReportCampaign(ctx, extraTn.CampaignID)
			if err != nil {
				t.Errorf("ReportCampaign of the control item failed (%s): %s", modclient.KindOf(err), e.raw.scrub(err.Error()))
				return
			}
			if m3aAllTerminal(rep, 1) {
				t.Logf("NOTE m3a edge run paused control: the same Run with dispatch_mode restored is a new job (202) and the item is %s", rep.Items[0].State)
				return
			}
			if time.Since(start) > 90*time.Second {
				t.Errorf("the control item did not finish within 90 s: %s", m3aTotalsLine(rep.Totals))
				return
			}
		}
	})

	// ---- the held queue: five items, the one lane held for 60 s after every push to a worker, the sink answering 451 ----
	// Module 3 stamps a lane's cooldown when a job is PUSHED to a worker, whatever the outcome (dispatch/loop.go, pacer.Stamp), so with one lane and a 60 s pause the first item is tried (the sink
	// answers 451: queued, attempt_count 1, transport_4xx, a 451 text, and, as the first record run showed, the account that attempt used still attached) and the other four are not tried at all
	// (queued, attempt_count 0, nothing else): the two shapes of a queued item, and a last page whose items have no account.
	var tried, untried []string // job item ids, as the adapter's campaign report shows them
	byAccount := map[string]bool{}
	pg, err := sendapi.New(e.mc, m3.Profile(), x.UserID, sendapi.Options{PageSize: 2})
	if err != nil {
		t.Fatalf("sendapi.New(page size 2): %s", e.raw.scrub(err.Error()))
	}
	step("held queue", func() {
		e.setPacing(t, x, 1, 60, 60)
		e.setMode(t, m3hMode{DeferRcpt: x.emails("")})
		took := e.runAll(t, x)
		start := time.Now()
		for {
			r, err := x.C.ReportCampaign(ctx, x.CampaignID)
			if err != nil {
				t.Errorf("ReportCampaign of the held campaign failed (%s): %s", modclient.KindOf(err), e.raw.scrub(err.Error()))
				return
			}
			tried, untried = nil, nil
			for _, it := range r.Items {
				if it.AttemptCount >= 1 && it.ErrorClass != nil && it.SMTPResponse != nil {
					tried = append(tried, it.JobItemID)
				} else {
					untried = append(untried, it.JobItemID)
				}
			}
			if len(tried) >= 1 {
				t.Logf("NOTE m3a edge held queue: %d items run in %s; after %s the report says %s: %d item(s) tried (the sink answered 451), %d not tried (the lane is held for 60 s)", len(x.Items), took.Round(time.Millisecond), time.Since(start).Round(time.Millisecond), m3aTotalsLine(r.Totals), len(tried), len(untried))
				return
			}
			if time.Since(start) > 60*time.Second {
				t.Errorf("after 60 s no item shows a failed attempt (%s): the sink's 451 did not reach module 3's report\n%s", m3aTotalsLine(r.Totals), e.diagnostics(t, x))
				return
			}
			time.Sleep(500 * time.Millisecond)
		}
	})
	if all, err := x.C.AllAccounts(ctx); err == nil {
		for _, a := range all {
			byAccount[a.ID] = true
		}
	}
	// account fields of a queued item: both set or both absent, and when set they name one of the tenant's own accounts (the module keeps the account of the last attempt on a requeued job: not asserted either way)
	accountOf := func(it sendapi.Item) string {
		if (it.AccountID == nil) != (it.AccountUsername == nil) {
			t.Errorf("a queued item carries account_id %s and account_username %s: both or neither", m3aText(it.AccountID), m3aText(it.AccountUsername))
		}
		if it.AccountID != nil && len(byAccount) != 0 && !byAccount[*it.AccountID] {
			t.Errorf("a queued item names account %s, which is none of the tenant's accounts", *it.AccountID)
		}
		if it.AccountID == nil {
			return "none"
		}
		return m3aShortID(it.AccountID)
	}
	step("job queued (tried)", func() {
		if len(tried) == 0 {
			t.Errorf("no item was tried: the single-job report of an item the sink answered 451 cannot be read")
			return
		}
		one, err := x.C.ReportJob(ctx, tried[0])
		if err != nil {
			t.Errorf("ReportJob of an item the sink answered 451 failed (%s): %s", modclient.KindOf(err), e.raw.scrub(err.Error()))
			return
		}
		if one.State != "queued" || one.Class != sendapi.ClassPending || one.AttemptCount < 1 || one.ErrorClass == nil || *one.ErrorClass != "transport_4xx" || one.SMTPResponse == nil || !strings.Contains(*one.SMTPResponse, "451") ||
			one.CompletedAt != nil || one.FailedAt != nil || one.ProviderMessageID != nil {
			t.Errorf("the single-job report of an item the sink answered 451 is state %s class %s attempt_count %d error_class %s smtp_response %s completed_at %v failed_at %v, want queued, pending, attempt_count >= 1, transport_4xx, a text naming 451, and no end",
				one.State, one.Class, one.AttemptCount, m3aText(one.ErrorClass), m3aText(one.SMTPResponse), one.CompletedAt, one.FailedAt)
			return
		}
		acct := accountOf(one)
		t.Logf("NOTE m3a edge job queued (tried): state %s attempt_count %d error_class %s smtp_response %q; the account of the failed attempt is still attached: %s", one.State, one.AttemptCount, *one.ErrorClass, *one.SMTPResponse, acct)
	})
	step("job queued (not tried)", func() {
		if len(untried) == 0 {
			t.Logf("NOTE m3a edge job queued (not tried): every item was tried, so there is no never-tried queued item to read (the lane's 60 s cooldown did not hold the others back)")
			return
		}
		one, err := x.C.ReportJob(ctx, untried[0])
		if err != nil {
			t.Errorf("ReportJob of an item module 3 has not tried failed (%s): %s", modclient.KindOf(err), e.raw.scrub(err.Error()))
			return
		}
		if one.State != "queued" || one.AttemptCount != 0 || one.ErrorClass != nil || one.SMTPResponse != nil || one.CompletedAt != nil || one.FailedAt != nil {
			t.Errorf("the single-job report of an item module 3 has not tried is state %s attempt_count %d error_class %s smtp_response %s, want queued, attempt_count 0 and nothing else", one.State, one.AttemptCount, m3aText(one.ErrorClass), m3aText(one.SMTPResponse))
			return
		}
		t.Logf("NOTE m3a edge job queued (not tried): state %s attempt_count %d, no error class, no provider text; account %s", one.State, one.AttemptCount, accountOf(one))
	})
	step("campaign pages", func() {
		var shape []string
		var lastNext string
		withAccount := 0
		st, err := pg.Walk(ctx, x.CampaignID, func(p sendapi.CampaignPage) error {
			shape = append(shape, fmt.Sprint(len(p.Items)))
			lastNext = p.NextCursor
			for _, it := range p.Items {
				if it.State != "queued" {
					t.Errorf("an item of the held campaign is %s, want queued", it.State)
				}
				if it.AccountID != nil {
					withAccount++
				}
				accountOf(it)
			}
			return nil
		})
		if err != nil || st.Pages != 3 || st.Items != 5 || strings.Join(shape, ",") != "2,2,1" || lastNext != "" {
			t.Errorf("the held campaign at page size 2 walked %+v pages [%s] (error %v, the last page's next cursor %q), want 3 pages of 2, 2 and 1 and no cursor after the last", st, strings.Join(shape, ","), err, lastNext)
			return
		}
		t.Logf("NOTE m3a edge campaign pages: 5 queued items at page size 2: pages of [%s], the last with no next cursor; %d of the 5 items carry an account", strings.Join(shape, ","), withAccount)
	})
	step("campaign cursors", func() {
		// Phase 4 A1: both cursors the module did not issue are ONE answer, the 400 validation_failed with the detail cursor/malformed (the forged one, a base64 cursor that is not a timestamp and an id,
		// was a 500 until module 3 validated it; the adapter no longer maps a 500 to ErrBadCursor, so only this 400 can be reported as one)
		forged := base64.RawURLEncoding.EncodeToString([]byte("forged|forged"))
		for _, c := range []struct{ name, what, cursor string }{
			{"cursor_malformed", "a cursor that is not base64", "!!!not base64!!!"},
			{"cursor_forged", "a base64 cursor that is not a timestamp and an id", forged},
		} {
			_, err := pg.ReportCampaignPage(ctx, x.CampaignID, c.cursor)
			if me := m3aWantAnswer(t, "ReportCampaignPage with "+c.what, err, modclient.Refused, 400, "validation_failed"); me != nil {
				if !errors.Is(err, sendapi.ErrBadCursor) || len(me.Details) != 1 || me.Details[0].Field != "cursor" || me.Details[0].Issue != "malformed" {
					t.Errorf("the %s campaign cursor answered %v (details %v), want ErrBadCursor with {cursor, malformed}", c.name, err, me.Details)
				}
				t.Logf("NOTE m3a edge campaign %s: HTTP %d code %s details %v", c.name, me.Status, me.Code, me.Details)
			}
		}
		if page, err := pg.ReportCampaignPage(ctx, x.CampaignID, ""); err != nil || len(page.Items) != 2 || page.NextCursor == "" { // the control: the first page without a cursor
			t.Errorf("control: the first page without a cursor answered %d item(s) and cursor %q (error %v), want 2 and a next cursor", len(page.Items), page.NextCursor, err)
		}
	})
	suspend := func(tn *m3aTenant) {
		t.Helper()
		status, body := e.raw.do(t, e.raw.m3URL, http.MethodPost, "/v1/admin/users/"+tn.UserID+"/suspend", e.raw.adminKey, map[string]string{"reason": "m3 adapter harness: the queued items of the contract edge cases must never be claimed again"})
		e.raw.expect(t, "POST /v1/admin/users/{"+tn.Name+"}/suspend", status, body, http.StatusOK, "")
	}
	suspend(x) // X is done: nothing of it is claimed again (the sink mode below replaces the 451 mode)

	// ---- events with a system row: the page of the events contract ----
	// The events of an account that was created and verified are rows with no flags before, a flags object after, no reason; the events contract's page also carries the rows module 3's own
	// dispatcher writes (actor system, flags before and after, a reason). One of those is cheap: a send whose AUTH the sink refuses is an auth_failure, and at autoquarantine_mode=shadow
	// (the seeded mode) module 3 records the quarantine it WOULD have made as an `updated` event with a reason (dispatch/account_effect.go, writeShadowQuarantineEvent). The account is the only
	// one of a tenant of its own, so the event is on it.
	step("events with a system row", func() {
		ev := e.seedTenant(t, "ev", []string{"authfail"})
		defer suspend(ev) // the item is retried every 30 s: the tenant is frozen when the step ends
		e.setPacing(t, ev, 1, 0, 0)
		acct := e.createVerified(t, ev, "1", e.sinkIP)
		e.setMode(t, m3hMode{FailAuth: true}) // every AUTH, the worker's at SEND time too, is answered 535
		defer e.setMode(t, m3hMode{})
		if res, err := ev.C.Run(ctx, ev.Items[0].ID); err != nil || !res.Created {
			t.Errorf("Run of the item whose AUTH the sink will refuse answered %+v (error %v), want a new job", res, err)
			return
		}
		start := time.Now()
		var job sendapi.Item
		for {
			one, err := ev.C.ReportJob(ctx, ev.Items[0].ID)
			if err != nil {
				t.Errorf("ReportJob of the item failed (%s): %s", modclient.KindOf(err), e.raw.scrub(err.Error()))
				return
			}
			job = one
			if one.AttemptCount >= 1 && one.ErrorClass != nil {
				break
			}
			if time.Since(start) > 60*time.Second {
				t.Errorf("after 60 s the item shows no failed attempt (state %s attempt_count %d)\n%s", one.State, one.AttemptCount, e.diagnostics(t, ev))
				return
			}
			time.Sleep(500 * time.Millisecond)
		}
		e.setMode(t, m3hMode{}) // the AUTH refusal is over: the retry (30 s) and the tenant is frozen below
		t.Logf("NOTE m3a edge auth failure: the item is %s attempt_count %d error_class %s smtp_response %s", job.State, job.AttemptCount, m3aText(job.ErrorClass), m3aText(job.SMTPResponse))
		time.Sleep(time.Second) // the outcome's event is written in the outcome's own transaction: a second is far more than it needs
		all, err := ev.C.AccountEvents(ctx, acct.ID, "")
		if err != nil {
			t.Errorf("AccountEvents through the adapter failed (%s): %s", modclient.KindOf(err), e.raw.scrub(err.Error()))
			return
		}
		var trail []string
		withFlags := 0
		for _, row := range all.Items {
			trail = append(trail, row.Actor+":"+row.Action)
			if row.From != nil {
				withFlags++
			}
		}
		if len(all.Items) < 2 {
			t.Errorf("the account has %d event(s) after a create, a verification and an auth failure: %v, want at least two", len(all.Items), trail)
			return
		}
		path := "/v1/send-byo/accounts/" + url.PathEscape(acct.ID) + "/events"
		pages, total, next := 0, 0, ""
		for pages < 12 {
			q := url.Values{"user_id": {ev.UserID}, "limit": {"1"}}
			if next != "" {
				q.Set("cursor", next)
			}
			resp, err := e.rawDo(m3aRawCall{label: "m3.edge.accounts.events", method: http.MethodGet, path: path, query: q})
			if err != nil {
				t.Errorf("page %d of the events (limit=1) failed: %s", pages+1, e.raw.scrub(err.Error()))
				return
			}
			n, nx := m3aPageOf(t, resp.Body)
			pages++
			total += n
			if nx == "" {
				break
			}
			next = nx
		}
		if total != len(all.Items) || pages != len(all.Items) {
			t.Errorf("the events at limit=1 are %d page(s) holding %d event(s); the adapter's own read holds %d: want one page per event and the same events", pages, total, len(all.Items))
		}
		t.Logf("NOTE m3a edge events: %d event(s) %v, %d with flags before (a system row); the raw walk at limit=1 read %d page(s) of 1 event, every one but the last with a next cursor", len(all.Items), trail, withFlags, pages)
		if withFlags == 0 {
			t.Logf("NOTE m3a edge events: no event carries flags before: module 3 wrote no shadow would-quarantine row for the auth failure (autoquarantine_mode is not shadow, or the row is not written for this class): the events contract's page will keep missing the system row")
		}
	})
}

// ---- helpers of the edge cases ----

// m3aRawCall is one call to module 3's Domain A made through the observed module client (the one every adapter of these tests is built over), for the answers the adapter never lets the platform ask for
// (it validates ids, pages and bodies before it sends): the service token and the options, so the observer, are the adapter's own.
type m3aRawCall struct {
	label  string // the route label: lower-case letters, digits, '_' and '.'
	method string
	path   string // already escaped
	query  url.Values
	body   any
	expect []int // the success statuses; empty means 200
}

func (e *m3aEnv) rawDo(c m3aRawCall) (*modclient.Response, error) {
	return e.mc.Do(e.ctx, modclient.Request{Module: modclient.M3, Route: c.label, Method: c.method, Path: c.path, Query: c.query, Body: c.body, Idempotent: c.method == http.MethodGet, Expect: c.expect})
}

// m3aWantAnswer returns the module's error when err is a modclient error of the given kind with the given HTTP status and (when not "") the module's error code; otherwise it reports, once, what the
// module answered instead (so that a wrong guess of the research names itself) and returns nil.
func m3aWantAnswer(t *testing.T, what string, err error, kind modclient.Kind, status int, code string) *modclient.Error {
	t.Helper()
	var me *modclient.Error
	if !errors.As(err, &me) {
		t.Errorf("%s answered %v, want the module's HTTP %d %q", what, err, status, code)
		return nil
	}
	if me.Kind != kind || me.Status != status || (code != "" && me.Code != code) {
		t.Errorf("%s: the module answered kind %s HTTP %d code %q (details %v, request %s), want kind %s HTTP %d code %q", what, me.Kind, me.Status, me.Code, me.Details, me.RequestID, kind, status, code)
		return nil
	}
	return me
}

// m3aPageOf reads the two things every list of module 3 answers (items and next_cursor): how many items, and the cursor ("" when null).
func m3aPageOf(t *testing.T, body []byte) (n int, next string) {
	t.Helper()
	var w struct {
		Items      []json.RawMessage `json:"items"`
		NextCursor *string           `json:"next_cursor"`
	}
	if err := json.Unmarshal(body, &w); err != nil {
		t.Errorf("a list answer is not {items, next_cursor}: %v", err)
		return 0, ""
	}
	if w.NextCursor != nil {
		next = *w.NextCursor
	}
	return len(w.Items), next
}

// withDial sets one dial of module 3 (the way setDial does: superuser SQL on send_byo.config), runs fn, and puts the ORIGINAL value back the moment fn returns (a deferred restore, so a step that
// dies in the middle restores it too). Module 3 reads each of these dials fresh at every request.
func (e *m3aEnv) withDial(t *testing.T, key, jsonValue string, fn func()) {
	t.Helper()
	var orig string
	if err := e.db.QueryRow(e.ctx, "SELECT value::text FROM send_byo.config WHERE key = $1", key).Scan(&orig); err != nil {
		t.Fatalf("read module 3's dial %s: %v", key, err)
	}
	tag, err := e.db.Exec(e.ctx, "UPDATE send_byo.config SET value = $2::jsonb WHERE key = $1", key, `{"v":`+jsonValue+`}`)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("write module 3's dial %s: %v (rows %d)", key, err, tag.RowsAffected())
	}
	t.Logf("NOTE m3a dial %s: %s -> {\"v\":%s} for one step", key, orig, jsonValue)
	defer func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(e.ctx), 20*time.Second)
		defer cancel()
		if _, err := e.db.Exec(ctx, "UPDATE send_byo.config SET value = $2::jsonb WHERE key = $1", key, orig); err != nil {
			t.Errorf("restore module 3's dial %s to %s: %v", key, orig, err)
		}
	}()
	fn()
}
