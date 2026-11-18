//go:build harness && m2worker

package harness

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/adapters/m2"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// Phase 3, chunk B6c for module 2: THE CONTRACT EDGE CASES. The drift gate (zz_m2_worker_contract_drift_test.go) compares every response module 2 gave an adapter whose options carry the session's observer
// with the recorded fixture of its contract, and is red for every contract no response was matched to unless the session's allowlist says why the harness cannot elicit it. The end-to-end tests reach the
// ordinary answers (10 of the 20 contracts in the first record run); this test makes the real module give the REST, each as the module's own answer and not as something the test built:
//
//	POST /v1/run  error validation_failed   a body with user_id empty, through the observed module client (the adapter validates ids itself and would never send it)
//	POST /v1/run  error unauthenticated     a client built with a random token
//	POST /v1/run  error conflict            a run of an item of ANOTHER campaign of the user while the first campaign's job is queued (409 user_campaign_active)
//	POST /v1/run  error paused              dispatch_mode refuse_new, set and restored through module 2's own operator API (intake refused 503 paused)
//	POST /v1/run  error internal_error      the module's own per-user campaign-start advisory lock held by a superuser transaction for longer than the module's 10 s database deadline: the run waits for the lock,
//	                                        the deadline passes, and the module answers 500 (advisory_lock_failed in its log); the lock is released and the SAME run then succeeds
//	GET /v1/report job queued               dispatch_mode accept_and_defer (dispatch stopped, intake open): the run is accepted and stays queued, reason dispatch_paused
//	GET /v1/report campaign error cursor_malformed   a cursor that is not base64url
//	GET /v1/report campaign error cursor_forged      a well-formed cursor (base64url of "text|id") whose time is not a time: the module's SQL cast fails, a 500
//	GET /v1/describe ok                     the adapter's Describe
//	POST /v1/cancel error validation_failed an empty campaign_id, through the observed module client
//
// Every call goes through a client whose options carry m2Drift.Observer() (newM2eEnv builds them; the bad-token client is built here with the same options), asserts the module's own answer (kind, HTTP status,
// error code, and the detail where the contract has one) with a failure that names what it got, never prints a credential, and has a PAIRED CONTROL: the same call with correct input succeeds (a replay of Run,
// a run of the same item once the pause is lifted, a genuine cursor, a cancel of an unknown campaign). Only answers some contract declares are elicited here: a route and status no contract declares would be
// an "undeclared dependency" of the gate. The two operator levers (dispatch_mode and the advisory lock) are restored in the test and again in a cleanup; no other test of the binary runs while they are set.
// Evidence lines start with `NOTE edge`.

// m2cRefusal asserts that err is the module's refusal of the given kind, HTTP status and error code, and returns it. It fails the test (Fatalf) with what the module answered instead.
func m2cRefusal(t *testing.T, r *m2wRun, what string, err error, kind modclient.Kind, status int, code string) *modclient.Error {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: the call succeeded, want the module's refusal (%s, HTTP %d, code %q)", what, kind, status, code)
	}
	var me *modclient.Error
	if !errors.As(err, &me) {
		t.Fatalf("%s: the error is not a modclient error, want the module's refusal (%s, HTTP %d, code %q): %s", what, kind, status, code, r.scrub(err.Error()))
	}
	if me.Kind != kind || me.Status != status || me.Code != code {
		t.Fatalf("%s: module 2 answered kind %s, HTTP %d, code %q, want kind %s, HTTP %d, code %q (the contract this call is meant to exercise): %s", what, me.Kind, me.Status, me.Code, kind, status, code, r.scrub(err.Error()))
	}
	return me
}

// m2cDetail reports whether the refusal names a detail (the module's field and issue words).
func m2cDetail(me *modclient.Error, field, issue string) bool {
	for _, d := range me.Details {
		if d.Field == field && d.Issue == issue {
			return true
		}
	}
	return false
}

func m2cDetails(me *modclient.Error) string {
	var parts []string
	for _, d := range me.Details {
		parts = append(parts, d.Field+"/"+d.Issue)
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ",")
}

// m2cRung reads dispatch_mode, module 2's dispatch ladder (serving, accept_and_defer, refuse_new, stopped), as the operator API holds it.
func m2cRung(t *testing.T, r *m2wRun) json.RawMessage {
	t.Helper()
	status, body := r.admin(t, http.MethodGet, "/v1/admin/config/dispatch_mode", nil)
	r.expect(t, "GET /v1/admin/config/dispatch_mode", status, body, http.StatusOK, "")
	var row struct {
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(body, &row); err != nil || len(row.Value) == 0 {
		t.Fatalf("GET /v1/admin/config/dispatch_mode is not a config row (%v): %s", err, r.snip(body, 300))
	}
	return row.Value
}

// m2cSetRung sets dispatch_mode through the operator API, reads it back and compares (a PUT that answers 200 and stores something else would make every later answer meaningless).
func m2cSetRung(t *testing.T, r *m2wRun, rung string) {
	t.Helper()
	raw := json.RawMessage(`"` + rung + `"`)
	status, body := r.admin(t, http.MethodPut, "/v1/admin/config/dispatch_mode", map[string]json.RawMessage{"value": raw})
	r.expect(t, "PUT /v1/admin/config/dispatch_mode "+rung, status, body, http.StatusOK, "")
	if got := m2cRungName(m2cRung(t, r)); got != rung {
		t.Fatalf("dispatch_mode reads back as %q after the PUT, want %q", got, rung)
	}
}

// m2cRungName is the rung's name out of the JSON value the operator API holds.
func m2cRungName(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "(not a string: " + string(raw) + ")"
	}
	return s
}

// m2cJobs counts module 2's own job rows of one job item: what a refused or failed run must have left behind (nothing).
func m2cJobs(t *testing.T, r *m2wRun, jobItemID string) int {
	t.Helper()
	var n int
	if err := r.db.QueryRow(r.ctx, "SELECT count(*) FROM send_managed.jobs WHERE job_item_id = $1::uuid", jobItemID).Scan(&n); err != nil {
		t.Fatalf("count module 2's job rows of item %s: %v", jobItemID, err)
	}
	return n
}

// m2cSeedOtherCampaign commits one more public.job_items row for the tenant under a NEW campaign id (the second campaign of the same user) and adds it to the tenant's items so that the sink view knows
// its address. It returns the item and the campaign id.
func m2cSeedOtherCampaign(t *testing.T, r *m2wRun, tn *m2wTenant) (*m2wItem, string) {
	t.Helper()
	var campaign string
	if err := r.db.QueryRow(r.ctx, "SELECT gen_random_uuid()::text").Scan(&campaign); err != nil {
		t.Fatalf("gen_random_uuid: %v", err)
	}
	it := &m2wItem{Index: len(tn.Items) + 1, Kind: "ok", Email: strings.ToLower(fmt.Sprintf("%s%s-ok-other@example.test", r.prefix(), tn.Name))}
	err := r.db.QueryRow(r.ctx, `INSERT INTO public.job_items (campaign_id, user_id, recipient_email, rendered_subject, rendered_body)
		VALUES ($1::uuid, $2::uuid, $3, $4, $5) RETURNING id::text`,
		campaign, tn.UserID, it.Email, fmt.Sprintf("[m2w %s] %s other campaign", r.runID, tn.Name), "<p>m2w other campaign</p>").Scan(&it.ID)
	if err != nil {
		t.Fatalf("seed public.job_items of the second campaign of %s: %v", tn.Name, err)
	}
	tn.Items = append(tn.Items, it)
	tn.byID[it.ID] = it
	return it, campaign
}

// m2cWaitSent polls the single-job report through the adapter until the item is terminal and requires it to be sent.
func m2cWaitSent(t *testing.T, r *m2wRun, ad *m2.Client, it *m2wItem, limit time.Duration) {
	t.Helper()
	start := time.Now()
	for {
		one, err := ad.ReportJob(r.ctx, it.ID)
		if err != nil {
			t.Fatalf("ReportJob of item %02d failed (%s): %s", it.Index, modclient.KindOf(err), r.scrub(err.Error()))
		}
		if one.Class == sendapi.ClassTerminal {
			if one.State != "sent" {
				t.Fatalf("item %02d ended %s (reason %s), want sent: the sink accepts every recipient of this test", it.Index, one.State, m2eText(one.Reason))
			}
			return
		}
		if time.Since(start) > limit {
			t.Fatalf("item %02d is still %s after %s although dispatch is serving and the worker is up\n%s", it.Index, one.State, limit, r.diagnostics(t, nil))
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func TestM2Worker_ContractEdgeCases(t *testing.T) {
	r := m2wNewRun(t)
	r.prepare(t)
	e := newM2eEnv(t, r)
	ctx := r.ctx

	// tenant "edge": campaign X holds items A, C, D and E; a fifth item B belongs to another campaign Y of the same user
	tn := r.newTenant(t, "edge", []string{"ok", "ok", "ok", "ok"}, true)
	itA, itC, itD, itE := tn.Items[0], tn.Items[1], tn.Items[2], tn.Items[3]
	itB, campaignY := m2cSeedOtherCampaign(t, r, tn)
	ad := e.client(t, tn)
	t.Logf("NOTE edge setup: tenant %s, campaign X %s (items A, C, D, E), campaign Y %s (item B); every recipient is accepted by the sink; no run has been made yet", tn.UserID, tn.CampaignID, campaignY)

	// the dispatch ladder is global and must never be left pulled: restored in the test and again here
	prior := m2cRung(t, r)
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(r.ctx), 30*time.Second)
		defer cancel()
		status, body, err := r.doCtx(cctx, r.m2URL, http.MethodPut, "/v1/admin/config/dispatch_mode", r.adminKey, map[string]json.RawMessage{"value": prior})
		if err != nil || status != http.StatusOK {
			t.Errorf("restoring dispatch_mode to %s failed (HTTP %d, %v): %s", prior, status, err, r.snip(body, 300))
		}
		// the second campaign is cancelled too (a no-op once its item is sent): prepare's cleanup knows only campaign X
		_, _, _ = r.doCtx(cctx, r.m2URL, http.MethodPost, "/v1/cancel", r.token, map[string]string{"campaign_id": campaignY, "user_id": tn.UserID})
	})
	if got := m2cRungName(prior); got != "serving" {
		t.Fatalf("dispatch_mode is %q at the start, want \"serving\": the harness leaves module 2 serving between tests", got)
	}

	// ---- GET /v1/report: a job that is QUEUED (dispatch stopped, intake open) ----
	m2cSetRung(t, r, "accept_and_defer")
	res, err := ad.Run(ctx, itA.ID)
	if err != nil || !res.Created || res.State != m2wStateQueued || res.Class != sendapi.ClassPending {
		t.Fatalf("Run of item A under accept_and_defer answered %+v (error %v), want a new queued job: this rung accepts and defers", res, err)
	}
	itA.JobID = res.JobID
	one, err := ad.ReportJob(ctx, itA.ID)
	if err != nil {
		t.Fatalf("ReportJob of the queued item A failed (%s): %s", modclient.KindOf(err), r.scrub(err.Error()))
	}
	if one.State != "queued" || one.Class != sendapi.ClassPending || one.JobID != itA.JobID || m2eText(one.Reason) != "dispatch_paused" || one.ErrorClass != nil {
		t.Errorf("the single-job report of item A is %s/%s class %s (reason %s, error class %s, job %s), want queued, class pending, reason dispatch_paused, no error class, job %s", one.State, m2eText(one.Reason), one.Class, m2eText(one.Reason), m2eText(one.ErrorClass), one.JobID, itA.JobID)
	}
	t.Logf("NOTE edge job queued: dispatch_mode accept_and_defer: Run of item A answered 202 queued (job %s); ReportJob says state %s, class pending, reason %s, error_class %s, attempt_count %d: the worker has not taken it and cannot while dispatch is deferred", itA.JobID, one.State, m2eText(one.Reason), m2eText(one.ErrorClass), one.AttemptCount)

	// ---- POST /v1/run: 409 conflict (a second campaign of the user while the first one's job is queued) ----
	_, err = ad.Run(ctx, itB.ID)
	me := m2cRefusal(t, r, "Run of item B (campaign Y) while item A of campaign X is queued", err, modclient.Refused, http.StatusConflict, "conflict")
	if !errors.Is(err, sendapi.ErrBackpressure) {
		t.Errorf("the 409 is not reported as sendapi.ErrBackpressure: %s", r.scrub(err.Error()))
	}
	if n := m2cJobs(t, r, itB.ID); n != 0 {
		t.Errorf("the refused Run of item B left %d job row(s) in module 2, want none", n)
	}
	t.Logf("NOTE edge run conflict: Run of item B (campaign Y) while A (campaign X) is queued answered kind %s, HTTP %d, code %s (request %s), reported as ErrBackpressure; module 2 holds no job for item B", me.Kind, me.Status, me.Code, me.RequestID)

	// ---- POST /v1/run: 503 paused (intake refused at refuse_new) ----
	m2cSetRung(t, r, "refuse_new")
	_, err = ad.Run(ctx, itC.ID)
	me = m2cRefusal(t, r, "Run of item C at dispatch_mode refuse_new", err, modclient.Paused, http.StatusServiceUnavailable, "paused")
	if n := m2cJobs(t, r, itC.ID); n != 0 {
		t.Errorf("the refused Run of item C left %d job row(s) in module 2, want none (the refusal comes before the job is created)", n)
	}
	t.Logf("NOTE edge run paused: dispatch_mode refuse_new: Run of item C answered kind %s, HTTP %d, code %s (request %s); module 2 holds no job for item C", me.Kind, me.Status, me.Code, me.RequestID)

	// ---- the controls of those three: the pause is lifted and the same calls succeed ----
	m2cSetRung(t, r, "serving")
	res, err = ad.Run(ctx, itC.ID)
	if err != nil || !res.Created || res.State != m2wStateQueued {
		t.Fatalf("CONTROL: Run of item C once dispatch serves again answered %+v (error %v), want a new queued job: the refusal was the pause and nothing else", res, err)
	}
	itC.JobID = res.JobID
	m2cWaitSent(t, r, ad, itA, 60*time.Second)
	m2cWaitSent(t, r, ad, itC, 60*time.Second)
	res, err = ad.Run(ctx, itB.ID)
	if err != nil || !res.Created {
		t.Fatalf("CONTROL: Run of item B once campaign X is finished answered %+v (error %v), want a new job: the conflict was the active campaign and nothing else", res, err)
	}
	itB.JobID = res.JobID
	m2cWaitSent(t, r, ad, itB, 60*time.Second)
	t.Logf("NOTE edge control pause and conflict: with dispatch serving again, Run of item C is a new job (202), items A and C are sent, and Run of item B is a new job (202) and is sent: neither refusal came from the input")

	// ---- POST /v1/run: 401 unauthenticated (a client with a token the module does not know) ----
	badOpts := modclient.FromConfig(e.cfg)
	badOpts.Observe = m2Drift.Observer()
	mcBad, err := modclient.New(modclient.M2, r.m2URL, config.Secret(randomHex(t, 24)), nil, badOpts)
	if err != nil {
		t.Fatalf("modclient.New with a random token: %s", r.scrub(err.Error()))
	}
	adBadAdapter, err := m2.NewAdapter(mcBad)
	if err != nil {
		t.Fatalf("m2.NewAdapter: %s", r.scrub(err.Error()))
	}
	adBad, err := adBadAdapter.For(tn.UserID)
	if err != nil {
		t.Fatalf("m2.Adapter.For with a random token: %s", r.scrub(err.Error()))
	}
	_, err = adBad.Run(ctx, itA.ID)
	me = m2cRefusal(t, r, "Run with a token module 2 does not know", err, modclient.Refused, http.StatusUnauthorized, "unauthenticated")
	t.Logf("NOTE edge run unauthenticated: Run of item A with a random token answered kind %s, HTTP %d, code %s (request %s)", me.Kind, me.Status, me.Code, me.RequestID)
	if rep, err := ad.Run(ctx, itA.ID); err != nil || rep.Created || rep.JobID != itA.JobID || rep.State != "sent" {
		t.Errorf("CONTROL: the same Run with the right token answered %+v (error %v), want the 200 replay of job %s in state sent", rep, err, itA.JobID)
	} else {
		t.Logf("NOTE edge control unauthenticated: the same Run with the right token is the 200 replay of job %s (state %s, created %v)", rep.JobID, rep.State, rep.Created)
	}

	// ---- POST /v1/run: 400 validation_failed (user_id empty; the adapter would refuse this body before sending it, so it goes through the observed module client) ----
	rawRun := func(user string) (*modclient.Response, error) {
		return e.mc.Do(ctx, modclient.Request{Module: modclient.M2, Route: "m2.run", Method: http.MethodPost, Path: "/v1/run", Idempotent: true, Expect: []int{http.StatusOK, http.StatusAccepted},
			Body: map[string]string{"job_item_id": itD.ID, "user_id": user}})
	}
	_, err = rawRun("")
	me = m2cRefusal(t, r, "Run with an empty user_id", err, modclient.Refused, http.StatusBadRequest, "validation_failed")
	if !m2cDetail(me, "user_id", "required") {
		t.Errorf("the 400 of Run names the details %s, want user_id/required", m2cDetails(me))
	}
	if n := m2cJobs(t, r, itD.ID); n != 0 {
		t.Errorf("the refused Run of item D left %d job row(s) in module 2, want none", n)
	}
	t.Logf("NOTE edge run validation_failed: Run of item D with an empty user_id answered kind %s, HTTP %d, code %s, details %s (request %s); module 2 holds no job for item D", me.Kind, me.Status, me.Code, m2cDetails(me), me.RequestID)
	resp, err := rawRun(tn.UserID)
	if err != nil || resp.Status != http.StatusAccepted {
		t.Fatalf("CONTROL: the same raw Run of item D with the user id did not answer 202 (error %v)", err)
	}
	var made struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(resp.Body, &made); err != nil || made.JobID == "" {
		t.Fatalf("CONTROL: the 202 of the raw Run of item D carries no job id (%v)", err)
	}
	itD.JobID = made.JobID
	m2cWaitSent(t, r, ad, itD, 60*time.Second)
	t.Logf("NOTE edge control validation_failed: the same body with the user id is accepted (202) and item D is sent")

	// ---- POST /v1/run: 500 internal_error (the module's per-user campaign-start lock is held longer than its 10 s database deadline) ----
	// createJob takes pg_advisory_xact_lock(hashtext('send_managed.campaign_start:' || user id)) before it writes anything. A superuser transaction holds the same lock; the run waits for it, the module's
	// 10 s deadline passes, and the module answers 500. Nothing is written (the lock comes first) and the lock is released by the rollback below.
	lockTx, err := r.db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the transaction that holds the module's campaign-start lock: %v", err)
	}
	held := true
	release := func() {
		if held {
			held = false
			_ = lockTx.Rollback(context.WithoutCancel(ctx))
		}
	}
	t.Cleanup(release)
	if _, err := lockTx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext('send_managed.campaign_start:' || $1::text))", tn.UserID); err != nil {
		t.Fatalf("take the module's campaign-start lock of the tenant: %v", err)
	}
	lockedAt := time.Now()
	_, err = ad.Run(ctx, itE.ID)
	waited := time.Since(lockedAt)
	release()
	me = m2cRefusal(t, r, "Run of item E while its campaign-start lock is held past the module's deadline", err, modclient.Internal, http.StatusInternalServerError, "internal_error")
	if waited < 8*time.Second {
		t.Errorf("the 500 came after %s, want about the module's 10 s database deadline: it is not the lock wait", waited.Round(time.Millisecond))
	}
	if n := m2cJobs(t, r, itE.ID); n != 0 {
		t.Errorf("the failed Run of item E left %d job row(s) in module 2, want none", n)
	}
	t.Logf("NOTE edge run internal_error: with the module's campaign-start lock of the tenant held by a superuser transaction, Run of item E answered kind %s, HTTP %d, code %s after %s (request %s); module 2 holds no job for item E", me.Kind, me.Status, me.Code, waited.Round(time.Millisecond), me.RequestID)
	res, err = ad.Run(ctx, itE.ID)
	if err != nil || !res.Created {
		t.Fatalf("CONTROL: the same Run of item E with the lock released answered %+v (error %v), want a new job", res, err)
	}
	itE.JobID = res.JobID
	m2cWaitSent(t, r, ad, itE, 60*time.Second)
	t.Logf("NOTE edge control internal_error: with the lock released the same Run of item E is a new job (202) and is sent")

	// ---- POST /v1/cancel: 400 validation_failed (empty campaign_id) ----
	_, err = e.mc.Do(ctx, modclient.Request{Module: modclient.M2, Route: "m2.cancel", Method: http.MethodPost, Path: "/v1/cancel", Idempotent: true,
		Body: map[string]string{"campaign_id": "", "user_id": tn.UserID}})
	me = m2cRefusal(t, r, "Cancel with an empty campaign_id", err, modclient.Refused, http.StatusBadRequest, "validation_failed")
	if !m2cDetail(me, "campaign_id", "required") {
		t.Errorf("the 400 of Cancel names the details %s, want campaign_id/required", m2cDetails(me))
	}
	t.Logf("NOTE edge cancel validation_failed: Cancel with an empty campaign_id answered kind %s, HTTP %d, code %s, details %s (request %s)", me.Kind, me.Status, me.Code, m2cDetails(me), me.RequestID)
	unknown := randomUUID(t, r)
	if pos, err := ad.Cancel(ctx, unknown); err != nil || !pos.Empty() {
		t.Errorf("CONTROL: Cancel of a campaign module 2 does not know answered %+v (error %v), want the 200 with all zeros", pos, err)
	} else {
		t.Logf("NOTE edge control cancel: Cancel of an unknown campaign answers 200 with all zeros (cancelled %d, in flight %d, already terminal %d): the same body with a campaign id is accepted", pos.Cancelled, pos.InFlight, pos.AlreadyTerminal)
	}

	// ---- GET /v1/report?campaign_id=&cursor=: 400 cursor_malformed and 400 cursor_forged (a 500 before the close-out) (campaign X is finished: four items, pages of one) ----
	rep, err := ad.ReportCampaign(ctx, tn.CampaignID)
	if err != nil || !m2eAllTerminal(rep, 4) {
		t.Fatalf("the report of campaign X is not four terminal items (%s), error %v", m2eTotalsLine(rep.Totals), err)
	}
	pg := e.pager(t, tn, 1)
	first, err := pg.ReportCampaignPage(ctx, tn.CampaignID, "")
	if err != nil || len(first.Items) != 1 || first.NextCursor == "" {
		t.Fatalf("the first page of campaign X at page size 1 is %d item(s) with next cursor %q (error %v), want one item and a cursor", len(first.Items), first.NextCursor, err)
	}
	_, err = pg.ReportCampaignPage(ctx, tn.CampaignID, "not base64 !")
	me = m2cRefusal(t, r, "the report with a cursor that is not base64", err, modclient.Refused, http.StatusBadRequest, "validation_failed")
	if !errors.Is(err, sendapi.ErrBadCursor) || !m2cDetail(me, "cursor", "malformed") {
		t.Errorf("the malformed cursor is reported as %s (ErrBadCursor %v), want sendapi.ErrBadCursor and the detail cursor/malformed", m2cDetails(me), errors.Is(err, sendapi.ErrBadCursor))
	}
	t.Logf("NOTE edge cursor_malformed: a cursor that is not base64url answered kind %s, HTTP %d, code %s, details %s (request %s), reported as ErrBadCursor", me.Kind, me.Status, me.Code, m2cDetails(me), me.RequestID)
	forged := base64.RawURLEncoding.EncodeToString([]byte("not-a-time|" + itA.ID))
	_, err = pg.ReportCampaignPage(ctx, tn.CampaignID, forged)
	// Phase 3 close-out B2 (module 2 dc2b532): a forged cursor is a 400 with the envelope, as the contract always said; before it module 2 answered 500 internal_error for a cursor whose time is not a time
	me = m2cRefusal(t, r, "the report with a well-formed cursor that is not a time", err, modclient.Refused, http.StatusBadRequest, "validation_failed")
	if !errors.Is(err, sendapi.ErrBadCursor) || !m2cDetail(me, "cursor", "malformed") {
		t.Errorf("the forged cursor's 400 is not reported as sendapi.ErrBadCursor with the detail cursor/malformed (%s): %s", m2cDetails(me), r.scrub(err.Error()))
	}
	t.Logf("NOTE edge cursor_forged: a well-formed cursor whose time is not a time answered kind %s, HTTP %d, code %s (request %s) and is reported as ErrBadCursor: a 400 since the close-out (it was a 500)", me.Kind, me.Status, me.Code, me.RequestID)
	second, err := pg.ReportCampaignPage(ctx, tn.CampaignID, first.NextCursor)
	if err != nil || len(second.Items) != 1 || second.Items[0].JobItemID == first.Items[0].JobItemID {
		t.Errorf("CONTROL: the genuine next cursor answered %d item(s) (error %v), want the next item of the campaign", len(second.Items), err)
	} else {
		t.Logf("NOTE edge control cursor: the genuine next_cursor of page one gives page two (item %s after item %s): the refusals above came from the cursors", second.Items[0].JobItemID, first.Items[0].JobItemID)
	}

	// ---- GET /v1/describe ----
	d, err := e.ad.Describe(ctx)
	if err != nil || d == nil || d.Module != "send_managed" {
		t.Fatalf("Describe through the adapter answered %+v (error %v), want module send_managed", d, err)
	}
	t.Logf("NOTE edge describe: GET /v1/describe answered 200 for module %s, api version %s", d.Module, d.APIVersion)

	// ---- the module and the sink end where they started: serving, and one message for each of the five items ----
	if got := m2cRungName(m2cRung(t, r)); got != m2cRungName(prior) {
		t.Errorf("dispatch_mode is %q at the end, want %q", got, m2cRungName(prior))
	}
	view := r.sinkView(t)
	r.checkSink(t, "edge", tn, view, func(it *m2wItem) m2wSinkWant { return m2wSinkWant{Messages: 1, Accepted: 1} })
	t.Logf("NOTE edge end: dispatch_mode is %s again; the sink holds exactly one message for each of items A, C, D, E and B (the refused and failed runs sent nothing, the replays sent nothing)", prior)
}

// randomUUID is a version 4 style uuid made by the database (a campaign id module 2 has never seen).
func randomUUID(t *testing.T, r *m2wRun) string {
	t.Helper()
	var id string
	if err := r.db.QueryRow(r.ctx, "SELECT gen_random_uuid()::text").Scan(&id); err != nil {
		t.Fatalf("gen_random_uuid: %v", err)
	}
	return id
}
