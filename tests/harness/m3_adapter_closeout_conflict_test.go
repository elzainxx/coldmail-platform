//go:build harness

package harness

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/coldmail-platform/platform/internal/adapters/m3"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// TestM3Adapter_RunConflictCausesAreNamed (Phase 3 close-out C2 / D5): module 3 answers a refused run with 409 `conflict` and, since the close-out, one details[] entry per cause; its quote answers from the same
// function. This test makes the real module refuse a run for each of the four causes, alone and combined, through the adapter, and requires for every refusal:
//
//	the quote   FeasibilityForCampaign, asked BEFORE the run for the campaign the item belongs to, is infeasible with exactly the causes the run is then refused for (its reasons are those causes and nothing else)
//	the error   Run returns a *sendapi.ConflictError: kind refused, HTTP 409, code conflict, route m3.run, exactly those causes in the module's order, errors.Is true for the sentinel of each cause named and false
//	            for the others, never ErrConflictUnexplained or ErrBackpressure, no result beside it, and NO job created at module 3
//	the control the same call with the cause cleared: the quote is feasible with no reason and the SAME Run is accepted (202, a new job, state queued)
//
// What is made live, and how (module 3's dispatch is held at the rung accept_and_defer for the whole test, so that every accepted job stays queued and the state is exactly what the test built; the rung is put
// back when the test ends, after every job of the tenant was cancelled through the adapter):
//
//	campaign_active  an item of campaign X1 is run (a job of ANOTHER campaign of the same user is not final); cleared by cancelling X1
//	suspended        the user is suspended through the platform's own admin client (m3admin, the path the reconcile loop uses); cleared by unsuspending. Combined with campaign_active: an item of a third campaign
//	                 is refused with BOTH, in the module's order
//	campaign_cap     module 3's max_campaign_size is lowered to what the campaign already holds (the dial run reads, set the way the other tests set dials and restored at once); cleared by restoring it. The quote
//	                 for zero recipients is still feasible at the cap and for one is not (a cap holds iff current + n <= limit)
//	queue_cap        max_queued_jobs lowered to the live queue the user holds; cleared by restoring it
//	both caps        both lowered at once: campaign_cap and queue_cap, in that order
//
// All four causes are made live; none is left out. The quote's other reasons (no eligible account, an unverified one, dispatch disabled, a blocked From) are not causes of a refused run and are not asked of.
func TestM3Adapter_RunConflictCausesAreNamed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), m3aBudget)
	t.Cleanup(cancel)
	e := newM3aEnv(t, ctx)
	e.prepare(t, true, true)

	tn := e.seedTenant(t, "conf", m3cKinds("ok", 6)) // campaign E: six items
	e.createVerified(t, tn, "1", e.sinkIP)           // an eligible account: the quote's only other verdicts would otherwise be no_eligible_accounts
	campE := tn.CampaignID
	campX1, campX2 := e.uuid(t), e.uuid(t) // two more campaigns of the same user, one item each
	x1 := e.seedItemsIn(t, tn, campX1, "x1")[0]
	x2 := e.seedItemsIn(t, tn, campX2, "x2")[0]
	items := tn.Items // e1..e6, campaign E

	refuse := func(step, campaign, itemID string, want ...sendapi.Cause) {
		t.Helper()
		fz, err := tn.C.FeasibilityForCampaign(ctx, campaign, 1)
		if err != nil {
			t.Errorf("%s: FeasibilityForCampaign failed (%s): %s", step, modclient.KindOf(err), e.raw.scrub(err.Error()))
			return
		}
		if fz.Feasible || !slices.Equal(fz.Causes(), want) || len(fz.Reasons) != len(want) {
			t.Errorf("%s: the quote is feasible %v with causes %v over %d reason(s), want infeasible with exactly %v and no other reason", step, fz.Feasible, fz.Causes(), len(fz.Reasons), want)
		}
		before := e.jobCount(t, tn.UserID, "")
		res, err := tn.C.Run(ctx, itemID)
		var re *modclient.Error
		if !errors.As(err, &re) || re.Kind != modclient.Refused || re.Status != 409 || re.Code != "conflict" || re.Route != "m3.run" || !m3aIsZero(res) {
			t.Errorf("%s: Run answered %+v and %v, want the module's 409 (refused, HTTP 409, code conflict, route m3.run) and no value", step, res, err)
			return
		}
		got := sendapi.CausesOf(err)
		if !slices.Equal(got, want) {
			t.Errorf("%s: the 409 names the causes %v, want exactly %v", step, got, want)
		}
		if !slices.Equal(got, fz.Causes()) {
			t.Errorf("%s: the 409 names %v and the quote predicted %v: quote and run must agree", step, got, fz.Causes())
		}
		for _, cause := range sendapi.RunCauses() {
			if errors.Is(err, cause.Sentinel()) != slices.Contains(want, cause) {
				t.Errorf("%s: errors.Is(err, the sentinel of %s) = %v, want %v", step, cause, !slices.Contains(want, cause), slices.Contains(want, cause))
			}
		}
		if errors.Is(err, sendapi.ErrConflictUnexplained) || errors.Is(err, sendapi.ErrBackpressure) || !errors.Is(err, modclient.ErrRefused) {
			t.Errorf("%s: the explained 409 is also unexplained or back-pressure (or not a refusal): %v", step, err)
		}
		if after := e.jobCount(t, tn.UserID, ""); after != before || e.jobsOf(t, tn.UserID)[itemID].ID != "" {
			t.Errorf("%s: module 3 holds %d job(s) of the tenant after the refused Run (%d before) and one for the item: a refused Run creates nothing", step, after, before)
		}
		t.Logf("NOTE m3a conflict %s: the quote predicted %v, Run was refused with %v (HTTP %d, request %s); no job created", step, fz.Causes(), got, re.Status, re.RequestID)
	}
	accept := func(step, campaign, itemID string) {
		t.Helper()
		fz, err := tn.C.FeasibilityForCampaign(ctx, campaign, 1)
		if err != nil || !fz.Feasible || len(fz.Reasons) != 0 || len(fz.Causes()) != 0 {
			t.Fatalf("%s (the control, the cause cleared): the quote is %+v (error %v), want feasible with no reason", step, fz, err)
		}
		res, err := tn.C.Run(ctx, itemID)
		if err != nil || !res.Created || res.State != "queued" || res.Class != sendapi.ClassPending || res.JobID == "" {
			t.Fatalf("%s (the control, the cause cleared): Run answered %+v (error %v), want a new job (202, queued)", step, res, err)
		}
	}
	campaignJobs := func(campaign string) int {
		return e.count(t, "count the jobs of a campaign", "SELECT count(*) FROM send_byo.jobs WHERE campaign_id = $1::uuid", campaign)
	}
	liveJobs := func() int {
		return e.count(t, "count the live jobs of the tenant", "SELECT count(*) FROM send_byo.jobs WHERE user_id = $1::uuid AND state::text IN ('queued','assigned','sending')", tn.UserID)
	}

	e.withDial(t, "dispatch_mode", `"accept_and_defer"`, func() {
		// the rung keeps intake open and stops sending: nothing of this test is claimed, so every job stays queued
		defer func() { // every job of the tenant is cancelled through the adapter before the rung is put back: nothing is left to be sent
			ctxc, cancelc := context.WithCancel(context.WithoutCancel(ctx))
			defer cancelc()
			for _, c := range []string{campE, campX1, campX2} {
				if _, err := tn.C.Cancel(ctxc, c); err != nil {
					t.Errorf("cleanup: Cancel of campaign %s failed (%s): %s", c, modclient.KindOf(err), e.raw.scrub(err.Error()))
				}
			}
			if live := liveJobs(); live != 0 {
				t.Errorf("cleanup: %d job(s) of the tenant are still live after every campaign was cancelled", live)
			}
		}()

		// ---- campaign_active: a job of ANOTHER campaign of the user is not final ----
		accept("setup: x1", campX1, x1.ID) // a first campaign is active
		refuse("campaign_active", campE, items[0].ID, sendapi.CauseCampaignActive)
		if pos, err := tn.C.Cancel(ctx, campX1); err != nil || pos.Cancelled != 1 || pos.InFlight != 0 || pos.AlreadyTerminal != 0 {
			t.Fatalf("cancel of campaign X1 answered %+v (error %v), want its one queued item cancelled", pos, err)
		}
		accept("campaign_active", campE, items[0].ID) // cleared: X1 is cancelled (final), the same item is accepted

		// ---- suspended, alone and with campaign_active ----
		if _, err := e.adm.SetSuspended(ctx, tn.UserID, true, "m3 adapter harness: the suspended cause of a refused run"); err != nil {
			t.Fatalf("suspend the tenant through the platform's admin client: %v", err)
		}
		refuse("suspended", campE, items[1].ID, sendapi.CauseSuspended) // E's own job is not "another campaign"
		refuse("suspended and campaign_active", campX2, x2.ID, sendapi.CauseSuspended, sendapi.CauseCampaignActive)
		if _, err := e.adm.SetSuspended(ctx, tn.UserID, false, ""); err != nil {
			t.Fatalf("unsuspend the tenant: %v", err)
		}
		accept("suspended", campE, items[1].ID) // cleared: the same item is accepted
		if got := campaignJobs(campE); got != 2 {
			t.Fatalf("campaign E holds %d job(s), want the two accepted (e1, e2)", got)
		}

		// ---- campaign_cap: the campaign holds what the dial allows ----
		e.withDial(t, "max_campaign_size", fmt.Sprint(campaignJobs(campE)), func() {
			if fz, err := tn.C.FeasibilityForCampaign(ctx, campE, 0); err != nil || !fz.Feasible || len(fz.Reasons) != 0 {
				t.Errorf("campaign_cap: the quote for zero more recipients at the cap is %+v (error %v), want feasible (a cap holds iff current + n <= limit)", fz, err)
			}
			refuse("campaign_cap", campE, items[2].ID, sendapi.CauseCampaignCap)
			fz, _ := tn.C.FeasibilityForCampaign(ctx, campE, 1)
			if len(fz.Reasons) == 1 && (fz.Reasons[0].Current == nil || fz.Reasons[0].Limit == nil || *fz.Reasons[0].Current != 2 || *fz.Reasons[0].Limit != 2 || fz.Reasons[0].CapName != "campaign_size_cap") {
				t.Errorf("campaign_cap: the reason is %+v, want current 2, limit 2 and the cap name campaign_size_cap", fz.Reasons[0])
			}
		})
		accept("campaign_cap", campE, items[2].ID) // cleared: the dial is back, the same item is accepted

		// ---- queue_cap: the user's live queue holds what the dial allows ----
		e.withDial(t, "max_queued_jobs", fmt.Sprint(liveJobs()), func() {
			refuse("queue_cap", campE, items[3].ID, sendapi.CauseQueueCap)
			fz, _ := tn.C.FeasibilityForCampaign(ctx, campE, 1)
			if len(fz.Reasons) == 1 && (fz.Reasons[0].Current == nil || fz.Reasons[0].Limit == nil || *fz.Reasons[0].Current != int64(liveJobs()) || *fz.Reasons[0].Limit != int64(liveJobs()) || fz.Reasons[0].Code != m3.InfeasibleExceedsQueueCap || fz.Reasons[0].CapName != "queue_depth_cap") {
				t.Errorf("queue_cap: the reason is %+v, want exceeds_queue_cap with current and limit %d and the cap name queue_depth_cap", fz.Reasons[0], liveJobs())
			}
		})
		accept("queue_cap", campE, items[3].ID)

		// ---- both caps at once: the module's order is campaign_cap, then queue_cap ----
		e.withDial(t, "max_campaign_size", fmt.Sprint(campaignJobs(campE)), func() {
			e.withDial(t, "max_queued_jobs", fmt.Sprint(liveJobs()), func() {
				refuse("campaign_cap and queue_cap", campE, items[4].ID, sendapi.CauseCampaignCap, sendapi.CauseQueueCap)
			})
		})
		accept("campaign_cap and queue_cap", campE, items[4].ID)

		// ---- the control of the whole test: with no cause the quote is feasible and the run is accepted, and the tenant holds exactly the jobs the accepted runs made ----
		if got := e.jobCount(t, tn.UserID, ""); got != 6 {
			t.Errorf("module 3 holds %d job(s) of the tenant, want 6: x1 (cancelled) and e1 to e5, and none for a refused run (x2 and e6 were never accepted)", got)
		}
		if jobs := e.jobsOf(t, tn.UserID); jobs[x2.ID].ID != "" || jobs[items[5].ID].ID != "" {
			t.Errorf("module 3 holds a job for an item whose run was refused and never retried")
		}
	})
	if live := liveJobs(); live != 0 {
		t.Errorf("%d job(s) of the tenant are live after the test: every one was to be cancelled", live)
	}
	e.requireBooks(t, "after the run conflict test")
}
