//go:build harness && m2worker

package harness

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	m2ad "github.com/coldmail-platform/platform/internal/adapters/m2"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// Phase 4, chunk D1, phase 1: module 2's side of the pending contracts (contract_pending_helpers_test.go says what a pending contract is and how a session treats it). The step runs inside the last
// test of the binary (TestM2Worker_ZContractDrift, in a subtest of its own so that a failure of the step cannot hide the session's verdict), against the REAL module 2 and its real Control, through
// adapters whose options carry a capture (not the session's observer):
//
//	POST /v1/pause    a campaign whose items sit queued (dispatch_mode accept_and_defer: intake open, nothing claimed): the answer says paused:true; and the same call again, the idempotent answer
//	POST /v1/resume   the same campaign: the answer says paused:false; and the same call again, the answer for a campaign that is not paused
//	POST /v1/run      a job item the platform marked cancelled (UPDATE public.job_items SET status = 'cancelled', as the superuser the session seeds with) and that was never run: 409 job_item_cancelled
//
// The recorded answers are the primary ones: the first pause (module 2's {campaign_id, paused, changed, state, queued, in_flight, terminal}), the first resume and the 409. The repeats have the
// shape of their route (a repeat of a pause differs from the first only in `changed`) and are compared with the same fixture in compare mode.
//
// Each call asserts what module 2 itself says (its state word, its `changed`, its counts) so that a step that recorded a no-op where it meant to hold something fails loudly; the campaign report
// says `paused` as the answer did; the controls are the repeats, a run of an unmarked item of the same campaign (which is accepted: only the marker made the difference), and the end state: the
// campaign is cancelled (nothing was ever claimed, so nothing was sent), the dispatch rung is back at `serving` and the sink holds not one message for the five recipients.
func m2PauseCases(t *testing.T) {
	t.Helper()
	const step = "module 2: pause, resume and a run of a cancelled item"
	cp := m2cp
	if !cp.anyArmed() {
		cp.noteSkipped(t, step)
		return
	}
	r := m2wNewRun(t)
	r.prepare(t)
	ctx := r.ctx

	// tenant "cpause": campaign X holds five items: A, B and C are run and held queued; D is marked cancelled by the platform and never run; E is the control, run last
	tn := r.newTenant(t, "cpause", []string{"ok", "ok", "ok", "ok", "ok"}, true)
	held, marked, control := tn.Items[:3], tn.Items[3], tn.Items[4]

	// the dispatch ladder is global and must never be left pulled: restored in the step and again here
	prior := m2cRung(t, r)
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(r.ctx), 30*time.Second)
		defer cancel()
		status, body, err := r.doCtx(cctx, r.m2URL, http.MethodPut, "/v1/admin/config/dispatch_mode", r.adminKey, map[string]json.RawMessage{"value": prior})
		if err != nil || status != http.StatusOK {
			t.Errorf("restoring dispatch_mode to %s failed (HTTP %d, %v): %s", prior, status, err, r.snip(body, 300))
		}
	})
	if got := m2cRungName(prior); got != "serving" {
		t.Fatalf("dispatch_mode is %q at the start, want \"serving\": the harness leaves module 2 serving between tests", got)
	}

	// the adapter of the step: the options the service builds module 2's client with, and the capture
	capt := cp.capture()
	cfg := config.Config{M2URL: r.m2URL, M2ServiceToken: r.token, ModuleTimeout: 20 * time.Second, ModuleRetries: 2, ModuleRetryBackoff: 250 * time.Millisecond}
	opts := modclient.FromConfig(cfg)
	opts.Observe = capt.Observe
	mc, err := modclient.New(modclient.M2, r.m2URL, r.token, nil, opts)
	if err != nil {
		t.Fatalf("modclient.New: %s", r.scrub(err.Error()))
	}
	ad, err := m2ad.NewAdapter(mc)
	if err != nil {
		t.Fatalf("m2ad.NewAdapter: %s", r.scrub(err.Error()))
	}
	c, err := ad.For(tn.UserID)
	if err != nil {
		t.Fatalf("m2.Adapter.For: %s", r.scrub(err.Error()))
	}
	t.Logf("NOTE cpause setup: tenant %s, campaign X %s (items A, B, C to be held; D to be marked cancelled and never run; E the control); every recipient is accepted by the sink; no run has been made yet", tn.UserID, tn.CampaignID)

	// ---- three items run and held: intake open, nothing claimed ----
	m2cSetRung(t, r, "accept_and_defer")
	for _, it := range held {
		res, err := c.Run(ctx, it.ID)
		if err != nil || !res.Created || res.State != m2wStateQueued || res.Class != sendapi.ClassPending {
			t.Fatalf("Run of item %02d under accept_and_defer answered %+v (error %v), want a new queued job: this rung accepts and defers", it.Index, res, err)
		}
		it.JobID = res.JobID
	}

	// ---- POST /v1/pause: the campaign is held ----
	pr, err := c.Pause(ctx, tn.CampaignID)
	o := capt.answer(t, "POST /v1/pause of campaign X", http.StatusOK)
	if err != nil || !pr.Paused || pr.CampaignID != tn.CampaignID {
		t.Fatalf("Pause of campaign X answered %+v (error %v), want the campaign paused: it holds %d queued item(s) and nothing is claimed under accept_and_defer", pr, err, len(held))
	}
	f := cpFields(t, o)
	if f["changed"] != true || f["state"] != "paused" || cpInt(f["queued"]) != int64(len(held)) || cpInt(f["in_flight"]) != 0 || cpInt(f["terminal"]) != 0 {
		t.Errorf("module 2's own words for the first pause: changed %v, state %v, queued %v, in_flight %v, terminal %v, want changed true, state paused, queued %d, in_flight 0, terminal 0", f["changed"], f["state"], f["queued"], f["in_flight"], f["terminal"], len(held))
	}
	cp.judge(t, cpKey(sendapi.RoutePause, "ok"), o, true)
	t.Logf("NOTE cpause pause: the first pause answered paused=true changed=%v state=%v queued=%v", f["changed"], f["state"], f["queued"])

	pr2, err := c.Pause(ctx, tn.CampaignID)
	o2 := capt.answer(t, "POST /v1/pause of campaign X again", http.StatusOK)
	if err != nil || !pr2.Paused || pr2.CampaignID != tn.CampaignID {
		t.Fatalf("the repeated Pause answered %+v (error %v), want the same answer: the campaign is already paused", pr2, err)
	}
	if f2 := cpFields(t, o2); f2["changed"] != false || f2["state"] != "paused" {
		t.Errorf("module 2's own words for the repeated pause: changed %v, state %v, want changed false (nothing written), state paused", f2["changed"], f2["state"])
	}
	cp.judge(t, cpKey(sendapi.RoutePause, "ok"), o2, false)

	rep, err := c.ReportCampaign(ctx, tn.CampaignID)
	if err != nil || !rep.Paused || rep.Totals.Total != int64(len(held)) {
		t.Fatalf("the campaign report while paused: paused %v, %d item(s), error %v, want paused with %d item(s): the report says what the pause answered", rep.Paused, rep.Totals.Total, err, len(held))
	}

	// ---- POST /v1/resume: the campaign is released ----
	rr, err := c.Resume(ctx, tn.CampaignID)
	o3 := capt.answer(t, "POST /v1/resume of campaign X", http.StatusOK)
	if err != nil || rr.Paused || rr.CampaignID != tn.CampaignID {
		t.Fatalf("Resume of campaign X answered %+v (error %v), want the campaign released (paused false)", rr, err)
	}
	if f3 := cpFields(t, o3); f3["changed"] != true || f3["state"] != "running" || cpInt(f3["queued"]) != int64(len(held)) {
		t.Errorf("module 2's own words for the first resume: changed %v, state %v, queued %v, want changed true, state running, queued %d", f3["changed"], f3["state"], f3["queued"], len(held))
	}
	cp.judge(t, cpKey(sendapi.RouteResume, "ok"), o3, true)

	rr2, err := c.Resume(ctx, tn.CampaignID)
	o4 := capt.answer(t, "POST /v1/resume of campaign X again", http.StatusOK)
	if err != nil || rr2.Paused || rr2.CampaignID != tn.CampaignID {
		t.Fatalf("the repeated Resume answered %+v (error %v), want the answer for a campaign that is not paused (paused false)", rr2, err)
	}
	if f4 := cpFields(t, o4); f4["changed"] != false || f4["state"] != "running" {
		t.Errorf("module 2's own words for the repeated resume: changed %v, state %v, want changed false (nothing to release), state running", f4["changed"], f4["state"])
	}
	cp.judge(t, cpKey(sendapi.RouteResume, "ok"), o4, false)

	rep, err = c.ReportCampaign(ctx, tn.CampaignID)
	if err != nil || rep.Paused {
		t.Fatalf("the campaign report after the resume: paused %v, error %v, want released", rep.Paused, err)
	}

	// ---- the held items are cancelled (nothing was ever claimed): the campaign ends as the other tests' campaigns end ----
	pos, err := c.Cancel(ctx, tn.CampaignID)
	if err != nil || pos.CampaignID != tn.CampaignID || pos.Cancelled != int64(len(held)) || pos.InFlight != 0 || pos.AlreadyTerminal != 0 {
		t.Fatalf("Cancel of campaign X answered %+v (error %v), want all %d queued item(s) cancelled and none in flight or terminal", pos, err, len(held))
	}

	// ---- POST /v1/run of an item the platform marked cancelled (and never ran): 409 job_item_cancelled, and no job ----
	tag, err := r.db.Exec(ctx, `UPDATE public.job_items SET status = 'cancelled' WHERE id = $1::uuid AND user_id = $2::uuid`, marked.ID, tn.UserID)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("marking job item D cancelled (as the superuser): %d row(s) updated, error %v, want exactly one", tag.RowsAffected(), err)
	}
	_, err = c.Run(ctx, marked.ID)
	o5 := capt.answer(t, "POST /v1/run of item D, marked cancelled", http.StatusConflict)
	me := m2cRefusal(t, r, "Run of item D, which the platform marked cancelled", err, modclient.Refused, http.StatusConflict, sendapi.CodeItemCancelled)
	if !errors.Is(err, sendapi.ErrItemCancelled) || errors.Is(err, sendapi.ErrBackpressure) || errors.Is(err, sendapi.ErrConflictUnexplained) {
		t.Errorf("the 409 is not reported as sendapi.ErrItemCancelled alone: %s", r.scrub(err.Error()))
	}
	if n := m2cJobs(t, r, marked.ID); n != 0 {
		t.Errorf("the refused Run of item D left %d job row(s) in module 2, want none", n)
	}
	cp.judge(t, cpKey(sendapi.RouteRun, "error job_item_cancelled"), o5, true)
	t.Logf("NOTE cpause run cancelled item: Run of item D, marked cancelled by the platform, answered kind %s, HTTP %d, code %s (request %s), reported as ErrItemCancelled; module 2 holds no job for it", me.Kind, me.Status, me.Code, me.RequestID)

	// the control: item E, of the same campaign and not marked, is accepted: only the marker made the difference
	res, err := c.Run(ctx, control.ID)
	if err != nil || !res.Created || res.State != m2wStateQueued {
		t.Fatalf("CONTROL: Run of item E (the same campaign, not marked) answered %+v (error %v), want a new queued job: the refusal was the marker and nothing else", res, err)
	}
	control.JobID = res.JobID
	if pos, err := c.Cancel(ctx, tn.CampaignID); err != nil || pos.Cancelled != int64(len(held))+1 || pos.InFlight != 0 {
		t.Fatalf("the second Cancel of campaign X answered %+v (error %v), want the control item cancelled too (%d cancelled in all) and none in flight", pos, err, len(held)+1)
	}

	// ---- the end: serving again, and nothing was sent to anyone ----
	m2cSetRung(t, r, "serving")
	time.Sleep(3 * time.Second) // more than the module's dispatch tick: a cancelled item that were claimed late would reach the sink here
	r.checkSink(t, "cpause", tn, r.sinkView(t), func(it *m2wItem) m2wSinkWant { return m2wSinkWant{} })
	t.Logf("NOTE cpause end: dispatch_mode is serving again; campaign X is cancelled (%d items cancelled, item D never run); the sink holds no message for any of the five recipients", len(held)+1)
}

// m2cp is the pending contracts of module 2's session: the three answers above, declared by the adapter package whether or not their recordings are installed.
var m2cp = newCPSet("m2", m2ad.PauseContracts(), m2ad.ContractFixtures(), "testdata/contract", m2ad.RecordedPauseFixtures(), m2Drift)
