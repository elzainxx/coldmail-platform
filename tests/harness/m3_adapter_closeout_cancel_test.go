//go:build harness

package harness

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi/sendapitest"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// TestM3Adapter_CancelStopsTheCampaign (Phase 3 close-out C1 / D5): module 3 has POST /v1/cancel, and the platform's adapter (sendapi.Client.Cancel, the same method as module 2's) drives it against the
// real module 3, its three workers and the controllable sink.
//
// THE SCENARIO. Tenant K runs a campaign of ten with ONE lane held for 60 s after every push (the pacing the contract edge cases use), and the sink holds every send in DATA for 6 s: module 3 hands exactly one
// item to a worker, the sink holds it, and the other nine stay queued. K then cancels through the adapter. The properties, each with its control:
//
//	the partition   cancelled + in_flight + already_terminal is the number of items (ten), at least one is cancelled and at least one is still in flight (the item the sink holds); the split is timing dependent and is not asserted
//	nothing new     no cancelled item ever reaches the sink (no RCPT, no message), observed from the moment the call returns until the campaign has been complete for longer than three dispatch ticks; the sink holds exactly
//	                one message for each item that was not cancelled and none for the others
//	reported once   the in-flight item is reported once, sent, and the campaign report (every page) equals module 3's own table state by state
//	settles free    every item of the report settles through Classify, the cancelled ones as the charge table's row M3-01 and the sent one as M3-99, and the platform's books gain nothing
//	idempotent      a second and a third cancel return the same position (the in-flight item moved to already terminal) and write nothing (no job event, no state changed); a repeat Run of a cancelled item is the
//	                module's 200 replay in the state cancelled, and the same call for the sent item answers sent (the control)
//	the stranger    tenant S cancels K's campaign and answers {0, 0, 0}: K's items are not touched (the control is K's own cancel, which does cancel)
//	the control     tenant C has the same campaign shape and is never cancelled: while its nine items wait in the queue a stranger's cancel changes nothing, and once C is released every item is sent exactly once
//
// The contract of the 400 (a missing campaign_id) is made through the observed module client, because the adapter validates its arguments before it sends them.
func TestM3Adapter_CancelStopsTheCampaign(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), m3aBudget)
	t.Cleanup(cancel)
	e := newM3aEnv(t, ctx)
	e.prepare(t, true, true)

	const n = 10
	k := e.seedTenant(t, "cxl", m3cKinds("ok", n)) // the cancelled campaign
	c := e.seedTenant(t, "ctl", m3cKinds("ok", n)) // the control: the same shape, never cancelled
	s := e.seedTenant(t, "strg", nil)              // the stranger: no campaign, no item
	e.createVerified(t, k, "1", e.sinkIP)
	e.createVerified(t, c, "1", e.sinkIP)
	e.setPacing(t, k, 1, 60, 60)
	e.setPacing(t, c, 1, 60, 60)

	// ---- K: ten items, one handed to a worker and held by the sink ----
	e.setMode(t, m3hMode{DataDelayMs: 6000})
	e.runAll(t, k)
	m3lWait(t, ctx, "module 3 hands one item to a worker and the sink holds its DATA", 45*time.Second, 100*time.Millisecond, func() (bool, string) {
		inf := e.inflight(t)
		return inf.Inflight >= 1, fmt.Sprintf("the sink holds %d session(s)", inf.Inflight)
	})
	before, err := k.C.ReportCampaign(ctx, k.CampaignID)
	if err != nil {
		t.Fatalf("ReportCampaign before the cancel failed (%s): %s", modclient.KindOf(err), e.raw.scrub(err.Error()))
	}
	if before.Totals.Total != n || before.Totals.Cancelled != 0 || before.Totals.Sent != 0 || e.jobCount(t, k.UserID, "cancelled") != 0 {
		t.Fatalf("before the cancel K's totals are (%s) and module 3 holds %d cancelled job(s), want %d items and none cancelled or sent", m3aTotalsLine(before.Totals), e.jobCount(t, k.UserID, "cancelled"), n)
	}

	// ---- the stranger: zeros, and K is untouched ----
	zero, err := s.C.Cancel(ctx, k.CampaignID)
	if err != nil || !zero.Empty() || zero.CampaignID != k.CampaignID {
		t.Fatalf("a stranger's cancel of K's campaign answered %+v (error %v), want {0, 0, 0} for the campaign asked and no error", zero, err)
	}
	if got := e.jobCount(t, k.UserID, "cancelled"); got != 0 {
		t.Fatalf("after a stranger's cancel module 3 holds %d cancelled job(s) of K, want none: another tenant's cancel must change nothing", got)
	}
	// the control of the stranger: a call with no campaign at all is the module's 400, typed, with no value (the adapter itself would not send it: the contract's 400 is made through the observed client)
	_, err = e.rawDo(m3aRawCall{label: "m3.closeout.cancel", method: http.MethodPost, path: "/v1/cancel", body: map[string]any{"user_id": k.UserID}})
	if me := m3aWantAnswer(t, "POST /v1/cancel with no campaign_id", err, modclient.Refused, 400, "validation_failed"); me != nil {
		if len(me.Details) != 1 || me.Details[0].Field != "campaign_id" || me.Details[0].Issue != "required" {
			t.Errorf("the cancel with no campaign answered details %v, want {campaign_id, required}", me.Details)
		}
	}

	// ---- K's own cancel ----
	cancelStart := time.Now()
	pos, err := k.C.Cancel(ctx, k.CampaignID)
	if err != nil {
		t.Fatalf("Cancel through the adapter failed (%s): %s", modclient.KindOf(err), e.raw.scrub(err.Error()))
	}
	atReturn := e.sinkView(t)
	if pos.CampaignID != k.CampaignID || pos.Total() != n || pos.Cancelled < 1 || pos.InFlight < 1 {
		t.Fatalf("Cancel answered %+v, want the position of K's %d items: cancelled + in flight + already terminal = %d, at least one cancelled and at least one in flight (the item the sink holds)", pos, n, n)
	}
	after, err := k.C.ReportCampaign(ctx, k.CampaignID)
	if err != nil {
		t.Fatalf("ReportCampaign after the cancel failed (%s): %s", modclient.KindOf(err), e.raw.scrub(err.Error()))
	}
	byID := map[string]*m3aItem{}
	for _, it := range k.Items {
		byID[it.ID] = it
	}
	var cancelled, kept []*m3aItem // the items module 3 cancelled, and the ones it did not (claimed before the cancel)
	for _, it := range after.Items {
		x, ok := byID[it.JobItemID]
		if !ok {
			t.Fatalf("the report after the cancel names the item %s, which is none of K's", it.JobItemID)
		}
		if it.State == "cancelled" {
			cancelled = append(cancelled, x)
		} else {
			kept = append(kept, x)
		}
	}
	if int64(len(cancelled)) != pos.Cancelled || after.Totals.Cancelled != pos.Cancelled || len(cancelled)+len(kept) != n {
		t.Fatalf("the report after the cancel has %d cancelled item(s) and totals.cancelled %d, the position says %d cancelled of %d items", len(cancelled), after.Totals.Cancelled, pos.Cancelled, n)
	}
	t.Logf("NOTE m3a cancel: the position of K's %d items after the call (in %s): cancelled %d, in flight %d, already terminal %d (a stranger's cancel of the same campaign: %+v)", n, time.Since(cancelStart).Round(time.Millisecond), pos.Cancelled, pos.InFlight, pos.AlreadyTerminal, zero)

	// ---- nothing new reaches the sink after the call returns ----
	reached := func(v m3aView) []string { // the cancelled items the sink saw anything of
		var p []string
		for _, it := range cancelled {
			if v.Counts[it.Email] != 0 || v.Rcpt[it.Email] != (m3hRcpt{}) {
				p = append(p, fmt.Sprintf("the cancelled item %02d (%s) reached the sink: %d message(s), RCPT %+v", it.Index, it.Email, v.Counts[it.Email], v.Rcpt[it.Email]))
			}
		}
		return p
	}
	m3aProblems(t, "nothing cancelled may reach the sink (at the moment the call returned)", reached(atReturn))
	forged := atReturn
	forged.Counts = map[string]int{cancelled[0].Email: 1}
	if len(reached(forged)) != 1 { // the control: the check sees a cancelled item that did reach the sink
		t.Fatal("the control of the sink check does not catch a cancelled item that reached the sink")
	}
	tick := time.Duration(e.dialInt(t, "dispatch_tick_interval_ms")) * time.Millisecond
	window := 3 * tick
	if window < 5*time.Second {
		window = 5 * time.Second
	}
	final := e.waitCampaign(t, k, 90*time.Second, func(sendapi.CampaignReport) {
		m3aProblems(t, "nothing cancelled may reach the sink (while the held item finishes)", reached(e.sinkView(t)))
	})
	completeAt := time.Now()
	for time.Since(completeAt) < window { // more than three dispatch ticks with the campaign complete: a cancelled item that were to be claimed late would show here
		m3aProblems(t, "nothing cancelled may reach the sink (after the campaign is complete)", reached(e.sinkView(t)))
		time.Sleep(500 * time.Millisecond)
	}
	view := e.sinkView(t)
	e.requireNoDups(t, view)

	// ---- reported once, and the sink holds one message for each item that was not cancelled ----
	m3aProblems(t, "K's report against what was submitted", sendapitest.ExactSet(m3aItemIDs(final.Items), k.ids()))
	if final.Totals.Total != n || final.Totals.Cancelled != pos.Cancelled || final.Totals.Sent != pos.InFlight+pos.AlreadyTerminal || final.Totals.Failed != 0 || final.Totals.Bounced != 0 ||
		final.Totals.InFlight != 0 || final.Totals.Pending != 0 || final.Totals.WaitingForCapacity != 0 || !final.Progress.IsComplete || !final.ItemsMatchTotal() {
		t.Errorf("K's final totals are (%s) complete %v, want %d items: %d cancelled (the position's), %d sent (what was in flight or already terminal) and nothing else", m3aTotalsLine(final.Totals), final.Progress.IsComplete, n, pos.Cancelled, pos.InFlight+pos.AlreadyTerminal)
	}
	if own := e.jobStates(e.jobsOf(t, k.UserID)); own != fmt.Sprintf("cancelled=%d sent=%d", pos.Cancelled, pos.InFlight+pos.AlreadyTerminal) {
		t.Errorf("module 3's own jobs of K are [%s], want cancelled=%d sent=%d", own, pos.Cancelled, pos.InFlight+pos.AlreadyTerminal)
	}
	for _, it := range final.Items {
		x := byID[it.JobItemID]
		isCancelled := false
		for _, cx := range cancelled {
			isCancelled = isCancelled || cx == x
		}
		switch {
		case isCancelled && (it.State != "cancelled" || it.Class != sendapi.ClassTerminal || it.ProviderMessageID != nil):
			t.Errorf("the cancelled item %02d is reported %s (class %s, provider id %s), want cancelled, terminal and no provider message id", x.Index, it.State, it.Class, m3aText(it.ProviderMessageID))
		case !isCancelled && (it.State != "sent" || it.ProviderMessageID == nil):
			t.Errorf("the item %02d that was in flight at the cancel is reported %s (provider id %s), want sent once, with its provider message id", x.Index, it.State, m3aText(it.ProviderMessageID))
		}
		msgs, rcpt := view.Counts[x.Email], view.Rcpt[x.Email]
		if isCancelled && (msgs != 0 || rcpt != (m3hRcpt{})) || !isCancelled && (msgs != 1 || rcpt.Accepted != 1 || rcpt.Rejected != 0) {
			t.Errorf("item %02d (cancelled %v): the sink holds %d message(s) and answered RCPT %+v, want none for a cancelled item and one message and one acceptance for the other", x.Index, isCancelled, msgs, rcpt)
		}
	}
	if view.of(k) != len(kept) {
		t.Errorf("the sink holds %d message(s) to K, want %d (one per item that was not cancelled)", view.of(k), len(kept))
	}

	// ---- settles free: the charge table says M3-01 for a cancelled item, and the books gain nothing ----
	cl, err := k.C.Classify(final.Items)
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	m3aProblems(t, "Classify of the cancelled campaign", m3aCompareClassified(final.Items, cl, m3aDecide))
	if len(cl.Settle) != n || len(cl.NotYet) != 0 || len(cl.Unmapped) != 0 {
		t.Errorf("Classify settles %d, waits for %d and flags %d of K's %d items, want all %d settled", len(cl.Settle), len(cl.NotYet), len(cl.Unmapped), n, n)
	}
	freeCancelled := 0
	for _, si := range cl.Settle {
		d := m3aDecide(si.Outcome)
		switch si.Outcome.State {
		case "cancelled":
			freeCancelled++
			if d.Kind != "free" || d.Unit != "" || d.Rule != "M3-01" {
				t.Errorf("the cancelled item %s settles as %+v, want free, no unit, by row M3-01", si.ItemRef, d)
			}
		case "sent":
			if d.Kind != "free" || d.Rule != "M3-99" {
				t.Errorf("the sent item %s settles as %+v, want free by row M3-99 (module 3 is not metered)", si.ItemRef, d)
			}
		default:
			t.Errorf("the item %s is settled in the state %s, want cancelled or sent", si.ItemRef, si.Outcome.State)
		}
	}
	if int64(freeCancelled) != pos.Cancelled {
		t.Errorf("%d cancelled item(s) settle free, the position says %d were cancelled", freeCancelled, pos.Cancelled)
	}
	// the control of the comparison: a copy of the report in which a cancelled item is not final is caught by the same function
	tampered := append([]sendapi.Item(nil), final.Items...)
	for i := range tampered {
		if tampered[i].State == "cancelled" {
			tampered[i].State, tampered[i].Class = "queued", sendapi.ClassPending
			break
		}
	}
	if len(m3aCompareClassified(tampered, cl, m3aDecide)) == 0 {
		t.Error("the control: m3aCompareClassified accepted a report whose cancelled item is not final but is in the settle list")
	}
	m3aProblems(t, "the books of K", m3aCompareBooks(e.books(t, k.UserID)))

	// ---- idempotent: the same position, nothing written ----
	events0, states0 := e.count(t, "count K's job events", "SELECT count(*) FROM send_byo.job_events WHERE user_id = $1::uuid", k.UserID), e.jobStates(e.jobsOf(t, k.UserID))
	pos2, err := k.C.Cancel(ctx, k.CampaignID)
	if err != nil {
		t.Fatalf("the second Cancel failed (%s): %s", modclient.KindOf(err), e.raw.scrub(err.Error()))
	}
	pos3, err := k.C.Cancel(ctx, k.CampaignID)
	if err != nil {
		t.Fatalf("the third Cancel failed (%s): %s", modclient.KindOf(err), e.raw.scrub(err.Error()))
	}
	if pos2.Cancelled != pos.Cancelled || pos2.InFlight != 0 || pos2.AlreadyTerminal != pos.AlreadyTerminal+pos.InFlight || pos2.Total() != n {
		t.Errorf("the second Cancel answered %+v, want the same position as the first %+v with the in-flight item now already terminal", pos2, pos)
	}
	if pos3 != pos2 {
		t.Errorf("the third Cancel answered %+v, the second %+v: a repeat must return the same position", pos3, pos2)
	}
	if events1, states1 := e.count(t, "count K's job events", "SELECT count(*) FROM send_byo.job_events WHERE user_id = $1::uuid", k.UserID), e.jobStates(e.jobsOf(t, k.UserID)); events1 != events0 || states1 != states0 {
		t.Errorf("two repeated cancels wrote: %d job events and [%s] before, %d and [%s] after, want nothing written", events0, states0, events1, states1)
	}
	m3aProblems(t, "the repeated cancel", m3aCompareCancelRepeat(pos, pos2, n))
	if len(m3aCompareCancelRepeat(pos, sendapi.CancelPosition{Cancelled: pos.Cancelled + 1, AlreadyTerminal: pos.InFlight + pos.AlreadyTerminal - 1, InFlight: 0}, n)) == 0 { // the control: a position that moved is caught
		t.Error("the control: m3aCompareCancelRepeat accepted a repeat that cancelled one more item")
	}
	// a repeat Run of a cancelled item is the module's replay, in the state cancelled; the same call for the item that was sent says sent (the control)
	cj := e.jobsOf(t, k.UserID)
	replay, err := k.C.Run(ctx, cancelled[0].ID)
	if err != nil || replay.Created || replay.State != "cancelled" || replay.Class != sendapi.ClassTerminal || replay.JobID != cj[cancelled[0].ID].ID {
		t.Errorf("a repeat Run of a cancelled item answered %+v (error %v), want the existing job %s with the state cancelled, not created", replay, err, cj[cancelled[0].ID].ID)
	}
	if r2, err := k.C.Run(ctx, kept[0].ID); err != nil || r2.Created || r2.State != "sent" || r2.JobID != cj[kept[0].ID].ID {
		t.Errorf("control: a repeat Run of the item that was sent answered %+v (error %v), want the existing job in the state sent", r2, err)
	}
	e.requireBooks(t, "after K's cancelled campaign")

	// ---- the control: the same shape, never cancelled; a stranger's cancel changes nothing; released, every item is sent once ----
	e.setMode(t, m3hMode{})
	e.runAll(t, c)
	m3lWait(t, ctx, "the first item of the control campaign reaches the sink", 45*time.Second, 200*time.Millisecond, func() (bool, string) {
		v := e.sinkView(t)
		return v.of(c) >= 1, fmt.Sprintf("the sink holds %d message(s) to C", v.of(c))
	})
	z2, err := s.C.Cancel(ctx, c.CampaignID)
	if err != nil || !z2.Empty() || z2.CampaignID != c.CampaignID {
		t.Fatalf("a stranger's cancel of the control campaign answered %+v (error %v), want {0, 0, 0}", z2, err)
	}
	mid, err := c.C.ReportCampaign(ctx, c.CampaignID)
	if err != nil || mid.Totals.Cancelled != 0 || mid.Totals.Total != n || e.jobCount(t, c.UserID, "cancelled") != 0 {
		t.Fatalf("after a stranger's cancel the control campaign is (%s), module 3 holds %d cancelled job(s) of C (error %v), want %d items and none cancelled", m3aTotalsLine(mid.Totals), e.jobCount(t, c.UserID, "cancelled"), err, n)
	}
	e.setPacing(t, c, 3, 0, 1) // released: the lane's hold is over for the rest of the campaign
	repC := e.waitCampaign(t, c, 240*time.Second, nil)
	m3aProblems(t, "the control campaign's report", m3aCompareReport(c, repC.Items))
	viewC := e.sinkView(t)
	m3aProblems(t, "the control campaign at the sink", m3aCompareSink(c, repC.Items, viewC, 0))
	if repC.Totals.Sent != n || repC.Totals.Cancelled != 0 || viewC.of(c) != n {
		t.Errorf("the control campaign ended (%s) with %d message(s) at the sink, want %d sent, none cancelled and %d messages: without a cancel every item is sent", m3aTotalsLine(repC.Totals), viewC.of(c), n, n)
	}
	m3aProblems(t, "the books of C", m3aCompareBooks(e.books(t, c.UserID)))
	e.requireBooks(t, "after the control campaign")
	t.Logf("NOTE m3a cancel control: C's campaign of %d (never cancelled, a stranger's cancel answered %+v) ended sent=%d cancelled=%d with %d message(s) at the sink; K's of %d ended sent=%d cancelled=%d with %d message(s)", n, z2, repC.Totals.Sent, repC.Totals.Cancelled, viewC.of(c), n, final.Totals.Sent, final.Totals.Cancelled, view.of(k))
}

// m3aCompareCancelRepeat is the property of a repeated cancel as a function of the two positions: the number of cancelled items and of items does not move, and what was in flight is not in flight any
// more (it finished). It returns problems so that the same function can be shown to catch a position that moved.
func m3aCompareCancelRepeat(first, repeat sendapi.CancelPosition, items int) []string {
	var p []string
	if repeat.Cancelled != first.Cancelled {
		p = append(p, fmt.Sprintf("a repeat cancelled %d item(s) where the first call left %d", repeat.Cancelled, first.Cancelled))
	}
	if repeat.Total() != int64(items) {
		p = append(p, fmt.Sprintf("the repeat accounts for %d item(s), want %d", repeat.Total(), items))
	}
	if repeat.InFlight > first.InFlight {
		p = append(p, fmt.Sprintf("the repeat has %d item(s) in flight, more than the first call's %d: a cancel started work", repeat.InFlight, first.InFlight))
	}
	return p
}
