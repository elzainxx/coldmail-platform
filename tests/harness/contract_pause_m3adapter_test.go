//go:build harness

package harness

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	m3ad "github.com/coldmail-platform/platform/internal/adapters/m3"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// Phase 4, chunk D1, phase 1: module 3's side of the pending contracts (contract_pending_helpers_test.go says what a pending contract is and how a session treats it). The step runs inside the last
// test of the binary (TestM3Adapter_ZContractDrift, in a subtest of its own so that a failure of the step cannot hide the session's verdict), against the REAL module 3, its three workers and the
// controllable sink, through an adapter whose options carry a capture (not the session's observer):
//
//	POST /v1/pause    a campaign with queued items (one lane held for 60 s after every push, the pacing the cancel tests use: the first item is sent at once, the others wait): the answer says
//	                  paused:true; and the same call again, the idempotent answer
//	POST /v1/resume   the same campaign: the answer says paused:false; and the same call again, the answer for a campaign that is not paused
//	POST /v1/run      a job item the platform marked cancelled (UPDATE public.job_items SET status = 'cancelled', as the superuser the session seeds with) and that was never run: 409 job_item_cancelled
//
// The recorded answers are the primary ones: the first pause (module 3's {campaign_id, state, paused}), the first resume and the 409. Module 3's answers to a pause and its repeat are the SAME bytes
// (it has no `changed`), so the session sees them once; the repeats are compared with the same fixture in compare mode.
//
// Each call asserts what module 3 itself says (its state word) so that a step that recorded a no-op where it meant to hold something fails loudly; the campaign report says `paused` as the
// answer did; the controls are the repeats, a run of an unmarked item of the same campaign (accepted: only the marker made the difference) and the end state: the campaign is cancelled and finished
// (every item terminal, none cancelled item at the sink, no duplicate message), the sink put back to its default mode by prepare's cleanup.
func m3PauseCases(t *testing.T) {
	t.Helper()
	const step = "module 3: pause, resume and a run of a cancelled item"
	cp := m3cp
	if !cp.anyArmed() {
		cp.noteSkipped(t, step)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), m3aBudget)
	t.Cleanup(cancel)
	e := newM3aEnv(t, ctx)
	e.prepare(t, true, true)

	// tenant "cpz": campaign X holds five items: A, B and C are run (the first is sent at once, the other two wait behind the held lane); D is marked cancelled by the platform and never run; E is the control
	k := e.seedTenant(t, "cpz", m3cKinds("ok", 5))
	e.createVerified(t, k, "1", e.sinkIP)
	e.setPacing(t, k, 1, 60, 60)
	held, marked, control := k.Items[:3], k.Items[3], k.Items[4]

	// the adapter of the step: the options the service builds module 3's client with, and the capture
	capt := cp.capture()
	mopts := modclient.FromConfig(e.cfg)
	mopts.Observe = capt.Observe
	mc, err := modclient.New(modclient.M3, e.cfg.M3URL, e.cfg.M3ServiceToken, nil, mopts)
	if err != nil {
		t.Fatalf("modclient.New: %s", e.raw.scrub(err.Error()))
	}
	ad, err := m3ad.NewAdapter(mc, m3ad.WithSuspensionReader(e.adm))
	if err != nil {
		t.Fatalf("m3.NewAdapter: %s", e.raw.scrub(err.Error()))
	}
	c := e.bind(t, k, ad)
	t.Logf("NOTE m3 cpz setup: tenant %s, campaign X %s (items A, B, C to be run and held; D to be marked cancelled and never run; E the control); one lane, held 60 s after every push", k.UserID, k.CampaignID)

	for _, it := range held {
		res, err := c.Run(ctx, it.ID)
		if err != nil || !res.Created || res.State != "queued" || res.Class != sendapi.ClassPending || res.JobID == "" {
			t.Fatalf("Run of item %02d through the adapter answered %+v (error %v), want a new job (Created) in state queued of class pending with a job id", it.Index, res, err)
		}
		it.JobID = res.JobID
	}

	// ---- POST /v1/pause: the campaign is held ----
	pr, err := c.Pause(ctx, k.CampaignID)
	o := capt.answer(t, "POST /v1/pause of campaign X", http.StatusOK)
	if err != nil || !pr.Paused || pr.CampaignID != k.CampaignID {
		t.Fatalf("Pause of campaign X answered %+v (error %v), want the campaign paused: it holds queued items behind a lane held for 60 s", pr, err)
	}
	if f := cpFields(t, o); f["state"] != "paused" || f["paused"] != true {
		t.Errorf("module 3's own words for the first pause: state %v, paused %v, want state paused and paused true", f["state"], f["paused"])
	}
	cp.judge(t, cpKey(sendapi.RoutePause, "ok"), o, true)

	pr2, err := c.Pause(ctx, k.CampaignID)
	o2 := capt.answer(t, "POST /v1/pause of campaign X again", http.StatusOK)
	if err != nil || !pr2.Paused || pr2.CampaignID != k.CampaignID {
		t.Fatalf("the repeated Pause answered %+v (error %v), want the same answer: the campaign is already paused", pr2, err)
	}
	if f2 := cpFields(t, o2); f2["state"] != "paused" || f2["paused"] != true {
		t.Errorf("module 3's own words for the repeated pause: state %v, paused %v, want the same as the first", f2["state"], f2["paused"])
	}
	cp.judge(t, cpKey(sendapi.RoutePause, "ok"), o2, false)

	rep, err := c.ReportCampaign(ctx, k.CampaignID)
	if err != nil || !rep.Paused || rep.Totals.Total != int64(len(held)) {
		t.Fatalf("the campaign report while paused: paused %v, %d item(s), error %v, want paused with %d item(s): the report says what the pause answered", rep.Paused, rep.Totals.Total, err, len(held))
	}

	// ---- POST /v1/resume: the campaign is released ----
	rr, err := c.Resume(ctx, k.CampaignID)
	o3 := capt.answer(t, "POST /v1/resume of campaign X", http.StatusOK)
	if err != nil || rr.Paused || rr.CampaignID != k.CampaignID {
		t.Fatalf("Resume of campaign X answered %+v (error %v), want the campaign released (paused false)", rr, err)
	}
	if f3 := cpFields(t, o3); f3["state"] != "running" || f3["paused"] != false {
		t.Errorf("module 3's own words for the first resume: state %v, paused %v, want state running and paused false", f3["state"], f3["paused"])
	}
	cp.judge(t, cpKey(sendapi.RouteResume, "ok"), o3, true)

	rr2, err := c.Resume(ctx, k.CampaignID)
	o4 := capt.answer(t, "POST /v1/resume of campaign X again", http.StatusOK)
	if err != nil || rr2.Paused || rr2.CampaignID != k.CampaignID {
		t.Fatalf("the repeated Resume answered %+v (error %v), want the answer for a campaign that is not paused (paused false)", rr2, err)
	}
	if f4 := cpFields(t, o4); f4["state"] != "running" || f4["paused"] != false {
		t.Errorf("module 3's own words for the repeated resume: state %v, paused %v, want state running and paused false", f4["state"], f4["paused"])
	}
	cp.judge(t, cpKey(sendapi.RouteResume, "ok"), o4, false)

	rep, err = c.ReportCampaign(ctx, k.CampaignID)
	if err != nil || rep.Paused {
		t.Fatalf("the campaign report after the resume: paused %v, error %v, want released", rep.Paused, err)
	}

	// ---- the campaign is cancelled (whatever was not claimed is cancelled, whatever was claimed finishes once) ----
	pos, err := c.Cancel(ctx, k.CampaignID)
	if err != nil || pos.CampaignID != k.CampaignID || pos.Total() != int64(len(held)) || pos.Cancelled < 1 {
		t.Fatalf("Cancel of campaign X answered %+v (error %v), want the position of its %d items with at least one cancelled (the lane is held for 60 s: the later items were never claimed)", pos, err, len(held))
	}

	// ---- POST /v1/run of an item the platform marked cancelled (and never ran): 409 job_item_cancelled, and no job ----
	jobsBefore := e.jobCount(t, k.UserID, "")
	tag, err := e.db.Exec(ctx, `UPDATE public.job_items SET status = 'cancelled' WHERE id = $1::uuid AND user_id = $2::uuid`, marked.ID, k.UserID)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("marking job item D cancelled (as the superuser): %d row(s) updated, error %v, want exactly one", tag.RowsAffected(), err)
	}
	_, err = c.Run(ctx, marked.ID)
	o5 := capt.answer(t, "POST /v1/run of item D, marked cancelled", http.StatusConflict)
	me := m3aWantAnswer(t, "Run of item D, which the platform marked cancelled", err, modclient.Refused, http.StatusConflict, sendapi.CodeItemCancelled)
	if !errors.Is(err, sendapi.ErrItemCancelled) || errors.Is(err, sendapi.ErrConflictUnexplained) || errors.Is(err, sendapi.ErrRunCampaignActive) || errors.Is(err, sendapi.ErrRunQueueCap) {
		t.Errorf("the 409 is not reported as sendapi.ErrItemCancelled alone (no cause of a conflict): %s", e.raw.scrub(err.Error()))
	}
	if n := e.jobCount(t, k.UserID, ""); n != jobsBefore {
		t.Errorf("the refused Run of item D changed module 3's job count of the tenant from %d to %d, want no job written", jobsBefore, n)
	}
	cp.judge(t, cpKey(sendapi.RouteRun, "error job_item_cancelled"), o5, true)
	if me != nil {
		t.Logf("NOTE m3 cpz run cancelled item: Run of item D, marked cancelled by the platform, answered kind %s, HTTP %d, code %s (request %s), reported as ErrItemCancelled; module 3 holds no job for it", me.Kind, me.Status, me.Code, me.RequestID)
	}

	// the control: item E, of the same campaign and not marked, is accepted: only the marker made the difference
	res, err := c.Run(ctx, control.ID)
	if err != nil || !res.Created || res.State != "queued" {
		t.Fatalf("CONTROL: Run of item E (the same campaign, not marked) answered %+v (error %v), want a new queued job: the refusal was the marker and nothing else", res, err)
	}
	control.JobID = res.JobID
	if pos, err := c.Cancel(ctx, k.CampaignID); err != nil || pos.Total() != int64(len(held))+1 || pos.Cancelled < 1 {
		t.Fatalf("the second Cancel of campaign X answered %+v (error %v), want the position of its %d items", pos, err, len(held)+1)
	}

	// ---- the end: every item that was run is terminal (the one in flight finished once), nothing cancelled reached the sink, no message twice ----
	run := append(append([]*m3aItem(nil), held...), control)
	byID := map[string]*m3aItem{}
	for _, it := range run {
		byID[it.ID] = it
	}
	start := time.Now()
	var final sendapi.CampaignReport
	for {
		final, err = c.ReportCampaign(ctx, k.CampaignID)
		if err != nil {
			t.Fatalf("ReportCampaign of campaign X failed (%s): %s", modclient.KindOf(err), e.raw.scrub(err.Error()))
		}
		if m3aAllTerminal(final, len(run)) {
			break
		}
		if time.Since(start) > 90*time.Second {
			t.Fatalf("campaign X did not finish within 90 s of its cancel: %s, is_complete %v\n%s", m3aTotalsLine(final.Totals), final.Progress.IsComplete, e.diagnostics(t, k))
		}
		time.Sleep(m3aPollEvery)
	}
	view := e.sinkView(t)
	e.requireNoDups(t, view)
	for _, it := range final.Items {
		x, ok := byID[it.JobItemID]
		if !ok {
			t.Errorf("the final report names the item %s, which is none of the items that were run", it.JobItemID)
			continue
		}
		want := 0
		if it.State == "sent" {
			want = 1
		}
		if got := view.Counts[x.Email]; got != want {
			t.Errorf("item %02d is reported %s and the sink holds %d message(s) for it, want %d: a cancelled item reaches no one and a sent item is sent once", x.Index, it.State, got, want)
		}
	}
	if got := view.Counts[marked.Email]; got != 0 {
		t.Errorf("the sink holds %d message(s) for item D, which was marked cancelled and never run, want none", got)
	}
	t.Logf("NOTE m3 cpz end: campaign X is finished (%s); item D was never run and the sink holds nothing for it", m3aTotalsLine(final.Totals))
}

// m3cp is the pending contracts of module 3's session: the three answers above, declared by the adapter package whether or not their recordings are installed.
var m3cp = newCPSet("m3", m3ad.PauseContracts(), m3ad.ContractFixtures(), "testdata/contract", m3ad.RecordedPauseFixtures(), m3Drift)
