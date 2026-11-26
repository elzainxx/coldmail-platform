package jobenginetest

import (
	"context"
	"errors"
	"testing"

	"github.com/coldmail-platform/platform/internal/charge"
	"github.com/coldmail-platform/platform/internal/jobengine"
	"github.com/coldmail-platform/platform/internal/jobs"
)

// The fake's pause, resume, claim and module 1 cancel are what the engine's database tests stand on (Phase 4 close-out D1): held to what the modules' contracts say, with a control for each.

func observeAll(t *testing.T, f *FakeModule, id string) (jobengine.Observation, map[string]charge.Outcome) {
	t.Helper()
	got := map[string]charge.Outcome{}
	obs, err := f.Observe(context.Background(), job(id), func(items []jobs.ResultItem) error {
		for _, it := range items {
			got[it.ItemRef] = it.Outcome
		}
		return nil
	})
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	return obs, got
}

func TestFake_PauseHoldsTheCampaign_TheClaimedItemFinishesAndNothingIsInvented(t *testing.T) {
	ctx := context.Background()
	f := &FakeModule{}
	rows := rowsOf(4)
	f.Submit(ctx, job("j1"), rows)
	if !f.Claim("j1", rows[0].ID) {
		t.Fatal("the worker could not claim a queued item of a campaign that is not held")
	}
	if err := f.Pause(ctx, job("j1")); err != nil {
		t.Fatal(err)
	}
	if !f.Paused("j1") {
		t.Fatal("the pause did not hold the campaign")
	}
	if f.Claim("j1", rows[1].ID) {
		t.Error("the worker claimed an item of a held campaign")
	}
	// the module's worker works: the item in flight finishes, the queued ones are NOT claimed and so not reported
	obs, got := observeAll(t, f, "j1")
	if !obs.Paused || obs.Complete || obs.Terminal != 1 || len(got) != 1 || got[rows[0].ID].State != StateSent {
		t.Fatalf("a held campaign after one observation: %+v, reported %v; want paused, one sent item, the other three untouched", obs, got)
	}
	for _, it := range f.Items("j1")[1:] {
		if it.State != StateQueued {
			t.Errorf("a held campaign changed an unclaimed item: %+v (a pause changes no item's state)", it)
		}
	}
	// a repeat of the pause changes nothing; the item is reported again, once per observation, never as another outcome
	if err := f.Pause(ctx, job("j1")); err != nil {
		t.Fatal(err)
	}
	if obs2, got2 := observeAll(t, f, "j1"); !obs2.Paused || len(got2) != 1 || got2[rows[0].ID] != got[rows[0].ID] {
		t.Errorf("a second pause changed the report: %+v / %v", obs2, got2)
	}
	// resume lets the same campaign go on, and the rest is worked and reported once each
	if err := f.Resume(ctx, job("j1")); err != nil {
		t.Fatal(err)
	}
	obs3, got3 := observeAll(t, f, "j1")
	if obs3.Paused || !obs3.Complete || len(got3) != 4 || obs3.Terminal != 4 {
		t.Errorf("after the resume: %+v, reported %d items; want complete and every item reported", obs3, len(got3))
	}
	if c := f.Calls(); c.Pause != 2 || c.Resume != 1 {
		t.Errorf("calls = %+v, want 2 pauses and 1 resume counted", c)
	}
	// the control: the same module, never paused, works everything on the first observation
	g := &FakeModule{}
	g.Submit(ctx, job("j1"), rows)
	if obs, got := observeAll(t, g, "j1"); obs.Paused || !obs.Complete || len(got) != 4 {
		t.Errorf("the control (never paused): %+v / %d items", obs, len(got))
	}
}

func TestFake_PauseAndResumeAreNoOpsWhereThereIsNothingToHoldAndAreIdempotent(t *testing.T) {
	ctx := context.Background()
	f := &FakeModule{}
	// an unknown campaign: nothing is held (a stored pause for nothing would silently hold the next item submitted under that id)
	if err := f.Pause(ctx, job("nobody")); err != nil || f.Paused("nobody") {
		t.Errorf("a pause of a campaign the module does not hold: %v, held %v", err, f.Paused("nobody"))
	}
	f.Submit(ctx, job("j1"), rowsOf(2))
	f.Pause(ctx, job("j1"))
	f.Resume(ctx, job("j1"))
	f.Resume(ctx, job("j1")) // a resume of a campaign that is not held
	if f.Paused("j1") {
		t.Error("a resume left the campaign held")
	}
	// a cancelled campaign is never held again (a pause never un-cancels)
	f.Cancel(ctx, job("j1"))
	f.Pause(ctx, job("j1"))
	if f.Paused("j1") {
		t.Error("a pause held a cancelled campaign")
	}
	// the control: an ordinary campaign IS held
	f.Submit(ctx, job("j2"), rowsOf(2))
	f.Pause(ctx, job("j2"))
	if !f.Paused("j2") {
		t.Error("the control campaign was not held")
	}
}

func TestFake_ACancelOfAHeldCampaignEndsTheHold_ModuleOneCancelsToo(t *testing.T) {
	ctx := context.Background()
	f := &FakeModule{Module: jobs.ModuleSendBYO}
	rows := rowsOf(3)
	f.Submit(ctx, job("j1"), rows)
	f.Claim("j1", rows[0].ID)
	f.Pause(ctx, job("j1"))
	if err := f.Cancel(ctx, job("j1")); err != nil {
		t.Fatal(err)
	}
	if f.Paused("j1") {
		t.Error("a cancel left the campaign held")
	}
	obs, got := observeAll(t, f, "j1")
	if obs.Paused || !obs.Complete || got[rows[0].ID].State != StateSent || got[rows[1].ID].State != StateCancelled || got[rows[2].ID].State != StateCancelled {
		t.Errorf("after the cancel of a held campaign: %+v / %v", obs, got)
	}
	// module 1 can cancel: its queued rows come back cancelled with ITS reason, the claimed one settles, and the sender's cancelled rows carry none
	m1 := &FakeModule{Module: jobs.ModuleVerify}
	res, _ := m1.Submit(ctx, job("j1"), rows)
	m1.ClaimInFlight("j1", 1)
	if err := m1.Cancel(ctx, job("j1")); err != nil {
		t.Fatalf("module 1 cancel: %v, want nil (module 1 has POST /v1/jobs/{id}/cancel)", err)
	}
	j := job("j1")
	j.ModuleRef = res.ModuleRef
	var m1got []jobs.ResultItem
	obs1, err := m1.Observe(ctx, j, func(p []jobs.ResultItem) error { m1got = append(m1got, p...); return nil })
	if err != nil || !obs1.Complete || obs1.Terminal != 3 {
		t.Fatalf("module 1 after the cancel: %+v, %v", obs1, err)
	}
	cancelled := 0
	for _, it := range m1got {
		if it.Outcome.State == StateCancelled {
			cancelled++
			if it.Outcome != (charge.Outcome{Module: "m1", State: "cancelled", Reason: "cancelled_by_caller"}) {
				t.Errorf("module 1's cancelled row is %+v, want {m1 cancelled cancelled_by_caller}", it.Outcome)
			}
		}
	}
	if cancelled != 2 {
		t.Errorf("module 1 reported %d cancelled rows, want 2 (the claimed one settled)", cancelled)
	}
	if o := f.reportedOf(Item{State: StateCancelled}); o.Reason != "" || o.Module != jobs.ModuleSendBYO {
		t.Errorf("a sender's cancelled row is %+v, want no reason", o)
	}
}

func TestFake_ModuleOneHasNoPauseAndIsNeverHeld(t *testing.T) {
	ctx := context.Background()
	m1 := &FakeModule{Module: jobs.ModuleVerify}
	res, _ := m1.Submit(ctx, job("j1"), rowsOf(2))
	for name, err := range map[string]error{"pause": m1.Pause(ctx, job("j1")), "resume": m1.Resume(ctx, job("j1"))} {
		if !errors.Is(err, jobengine.ErrNotSupported) {
			t.Errorf("module 1 %s: %v, want ErrNotSupported", name, err)
		}
	}
	j := job("j1")
	j.ModuleRef = res.ModuleRef
	if obs, err := m1.Observe(ctx, j, func([]jobs.ResultItem) error { return nil }); err != nil || obs.Paused {
		t.Errorf("module 1's report: %+v, %v; never held", obs, err)
	}
}

func TestFake_TheLagKnobsAnswerSuccessAndHoldNothingUntilTheNthCall(t *testing.T) {
	ctx := context.Background()
	f := &FakeModule{PauseAckAfter: 3, ResumeAckAfter: 2}
	f.Submit(ctx, job("j1"), rowsOf(3))
	for i := 1; i <= 3; i++ {
		if err := f.Pause(ctx, job("j1")); err != nil {
			t.Fatalf("pause %d: %v (a lagging module answers success)", i, err)
		}
		if held := f.Paused("j1"); held != (i == 3) {
			t.Errorf("after pause call %d the campaign is held = %v, want %v", i, held, i == 3)
		}
	}
	f.Resume(ctx, job("j1"))
	if !f.Paused("j1") {
		t.Error("the first resume of a module that acknowledges at the second let go")
	}
	f.Resume(ctx, job("j1"))
	if f.Paused("j1") {
		t.Error("the second resume did not let go")
	}
	// the control: the default module holds at the first call
	g := &FakeModule{}
	g.Submit(ctx, job("j1"), rowsOf(1))
	g.Pause(ctx, job("j1"))
	if !g.Paused("j1") {
		t.Error("the default module did not hold at the first call")
	}
}

func TestFake_ClaimRefusesWhatTheModulesRulesRefuse(t *testing.T) {
	ctx := context.Background()
	f := &FakeModule{}
	rows := rowsOf(3)
	f.Submit(ctx, job("j1"), rows)
	for name, c := range map[string]struct {
		job, ref string
	}{"an unknown campaign": {"nobody", rows[0].ID}, "an item the module does not hold": {"j1", "ffffffff-ffff-4fff-8fff-ffffffffffff"}} {
		if f.Claim(c.job, c.ref) {
			t.Errorf("a claim of %s succeeded", name)
		}
	}
	if !f.Claim("j1", rows[0].ID) || f.Claim("j1", rows[0].ID) {
		t.Error("a queued item is claimed once, and an item in flight cannot be claimed again")
	}
	f.Cancel(ctx, job("j1"))
	if f.Claim("j1", rows[1].ID) {
		t.Error("the worker claimed an item of a cancelled campaign")
	}
}

func TestFake_PauseAndResumeUnderAnOutageAreErrWaitAndChangeNothing(t *testing.T) {
	ctx := context.Background()
	f := &FakeModule{}
	f.Submit(ctx, job("j1"), rowsOf(2))
	f.SetDown(true)
	if err := f.Pause(ctx, job("j1")); !errors.Is(err, jobengine.ErrWait) || f.Paused("j1") {
		t.Errorf("pause while down: %v (held %v), want ErrWait and nothing held", err, f.Paused("j1"))
	}
	f.SetDown(false)
	f.Pause(ctx, job("j1"))
	f.SetDown(true)
	if err := f.Resume(ctx, job("j1")); !errors.Is(err, jobengine.ErrWait) || !f.Paused("j1") {
		t.Errorf("resume while down: %v (held %v), want ErrWait and the campaign still held", err, f.Paused("j1"))
	}
}
