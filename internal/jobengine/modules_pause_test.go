package jobengine_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/coldmail-platform/platform/internal/charge"
	"github.com/coldmail-platform/platform/internal/jobengine"
	"github.com/coldmail-platform/platform/internal/jobs"
)

// Phase 4 close-out D1: what the REAL wrappers send and answer for pause, resume and cancel, and what they read of the report. The stand-ins (modules_rig_test.go) are HAND-WRITTEN from the
// modules' Docs/api-contract.md at their new heads (no recording of the new routes exists yet): every test here that depends on one is named HandWritten, and the live tests of the harness replace
// them against the real modules after the pin. The contract both the fake and these wrappers satisfy (jobenginetest.ModuleContract) is run in modules_contract_test.go.

// observed runs one Observe of the job and returns what the module said and what it handed over.
func observed(t *testing.T, mod jobengine.Module, j jobs.EngineJob) (jobengine.Observation, map[string]jobs.ResultItem) {
	t.Helper()
	got := map[string]jobs.ResultItem{}
	obs, err := mod.Observe(ctxT(t), j, collect(got))
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	return obs, got
}

// requestsTo counts the stand-in's requests of a method and path.
func requestsTo(rig *senderRig, method, path string) int {
	n := 0
	for _, r := range rig.stub.Requests() {
		if r.Method == method && r.Path == path {
			n++
		}
	}
	return n
}

func TestSenders_PauseAndResumeHandWritten_TheCallsGoToTheRoutesAndTheReportSaysWhetherTheCampaignIsHeld(t *testing.T) {
	for _, module := range senders {
		t.Run(module, func(t *testing.T) {
			rig, _, mod := senderModule(t, module, 2)
			rows := jobenginetestRows(3)
			j := jobOf(module, 3)
			if _, err := mod.Submit(ctxT(t), j, rows); err != nil {
				t.Fatal(err)
			}
			if obs, _ := observed(t, mod, j); obs.Paused {
				t.Fatal("a campaign nobody paused is reported held")
			}
			if err := mod.Pause(ctxT(t), j); err != nil {
				t.Fatalf("pause: %v", err)
			}
			if rig.pauses.Load() != 1 || requestsTo(rig, "POST", "/v1/pause") != 1 {
				t.Fatalf("the pause route was reached %d time(s), want once", rig.pauses.Load())
			}
			// the body is cancel's: the campaign (the platform job id) and the tenant, nothing else
			var body map[string]string
			for _, r := range rig.stub.Requests() {
				if r.Path == "/v1/pause" {
					if err := json.Unmarshal(r.Body, &body); err != nil {
						t.Fatal(err)
					}
				}
			}
			if len(body) != 2 || body["campaign_id"] != rigCampaign || body["user_id"] != rigUser {
				t.Errorf("the pause body is %v, want exactly campaign_id and user_id", body)
			}
			obs, got := observed(t, mod, j)
			if !obs.Paused || obs.Complete || len(got) != 0 {
				t.Errorf("the report of a held campaign: %+v, %d item(s) handed over; want paused, not complete, nothing handed over (no item changed state)", obs, len(got))
			}
			// the capability was read ONCE from the module's describe and is remembered: a second call reads it no more, and sends the pause again (idempotent, never an error)
			if err := mod.Pause(ctxT(t), j); err != nil || rig.pauses.Load() != 2 || rig.describes.Load() != 1 {
				t.Errorf("a second pause: %v, %d pause(s), %d describe(s); want nil, 2 and 1", err, rig.pauses.Load(), rig.describes.Load())
			}
			if err := mod.Resume(ctxT(t), j); err != nil || rig.resumes.Load() != 1 || rig.describes.Load() != 1 {
				t.Errorf("resume: %v, %d resume(s), %d describe(s); want nil, 1 and 1 (the capability is not asked again)", err, rig.resumes.Load(), rig.describes.Load())
			}
			if obs, _ := observed(t, mod, j); obs.Paused {
				t.Error("after the resume the report still says paused")
			}
			if err := mod.Resume(ctxT(t), j); err != nil { // a resume of a campaign that is not held: a no-op, never an error
				t.Errorf("a second resume: %v", err)
			}
		})
	}
}

// A module whose describe does not declare the capability (a module older than the routes) is never sent the call: ErrNotSupported with nothing on the pause route, and a NO is not remembered.
func TestSenders_PauseWithoutTheDescribedCapabilityIsNotSupportedAndSendsNothingHandWritten(t *testing.T) {
	for _, module := range senders {
		t.Run(module, func(t *testing.T) {
			rig, _, mod := senderModule(t, module, 2)
			rig.noPause.Store(true)
			j := jobOf(module, 2)
			if _, err := mod.Submit(ctxT(t), j, jobenginetestRows(2)); err != nil {
				t.Fatal(err)
			}
			for name, call := range map[string]func() error{"pause": func() error { return mod.Pause(ctxT(t), j) }, "resume": func() error { return mod.Resume(ctxT(t), j) }} {
				err := call()
				if !errors.Is(err, jobengine.ErrNotSupported) || errors.Is(err, jobengine.ErrWait) {
					t.Errorf("%s of a module whose describe has no %s: %v, want ErrNotSupported", name, rig.capName(), err)
				}
			}
			if rig.pauses.Load() != 0 || rig.resumes.Load() != 0 {
				t.Errorf("the routes were reached (%d pause, %d resume) although the describe does not declare them", rig.pauses.Load(), rig.resumes.Load())
			}
			if rig.describes.Load() != 2 {
				t.Errorf("%d describe(s): a negative answer must be asked again at the next call (it is not remembered), want 2", rig.describes.Load())
			}
			// the control: the same module, the capability declared, is sent the call
			rig.noPause.Store(false)
			if err := mod.Pause(ctxT(t), j); err != nil || rig.pauses.Load() != 1 {
				t.Errorf("with the capability declared: %v, %d pause(s), want nil and 1", err, rig.pauses.Load())
			}
			// a describe that cannot be read waits (the platform's own question to the module, never a verdict about it)
			rig2, _, mod2 := senderModule(t, module, 2)
			rig2.down.Store(true)
			expectWait(t, "pause when the describe cannot be read", mod2.Pause(ctxT(t), jobOf(module, 2)))
		})
	}
}

// What a module's refusal of the call is: a wait, never a refusal for good and never a failure of the job.
func TestSenders_APauseTheModuleFailsOrCannotAnswerIsAWaitHandWritten(t *testing.T) {
	for _, module := range senders {
		t.Run(module, func(t *testing.T) {
			rig, _, mod := senderModule(t, module, 2)
			j := jobOf(module, 2)
			if _, err := mod.Submit(ctxT(t), j, jobenginetestRows(2)); err != nil {
				t.Fatal(err)
			}
			rig.pauseAnswers.Store(1) // the next pause answers 500
			expectWait(t, "a pause answered 500", mod.Pause(ctxT(t), j), "pause")
			if err := mod.Pause(ctxT(t), j); err != nil { // the control: the next one is fine
				t.Errorf("the pause after the failed one: %v", err)
			}
			rig.down.Store(true)
			expectWait(t, "a pause while the module is down", mod.Pause(ctxT(t), j))
			expectWait(t, "a resume while the module is down", mod.Resume(ctxT(t), j))
			rig.down.Store(false)
			if obs, _ := observed(t, mod, j); !obs.Paused {
				t.Error("what was held before the outage is not held after it")
			}
		})
	}
}

// Observation.Paused is the CAMPAIGN's stored pause (the report's `paused`), never the operator's dispatch_paused: paired controls in both directions.
func TestSenders_ObservationPausedIsTheReportsPausedAndNeverDispatchPausedHandWritten(t *testing.T) {
	for _, module := range senders {
		t.Run(module, func(t *testing.T) {
			rig, _, mod := senderModule(t, module, 2)
			j := jobOf(module, 2)
			if _, err := mod.Submit(ctxT(t), j, jobenginetestRows(2)); err != nil {
				t.Fatal(err)
			}
			rig.dispatchPaused.Store(true) // the operator's rung: the module is not sending for anybody, and this campaign is not held
			obs, _ := observed(t, mod, j)
			if obs.Paused || obs.Detail["dispatch_paused"] != true {
				t.Errorf("the operator's rung is %+v: Paused must be false (this campaign is not held), and the detail still says what the module said", obs)
			}
			if err := mod.Pause(ctxT(t), j); err != nil {
				t.Fatal(err)
			}
			if obs, _ := observed(t, mod, j); !obs.Paused || obs.Detail["paused"] != true {
				t.Errorf("a held campaign under the operator's rung: %+v, want Paused", obs)
			}
			rig.dispatchPaused.Store(false) // the other direction: held, and the operator's rung is serving
			if obs, _ := observed(t, mod, j); !obs.Paused || obs.Detail["dispatch_paused"] != false {
				t.Errorf("a held campaign with no operator rung: %+v, want Paused and dispatch_paused false", obs)
			}
			if err := mod.Resume(ctxT(t), j); err != nil {
				t.Fatal(err)
			}
			if obs, _ := observed(t, mod, j); obs.Paused {
				t.Errorf("a campaign that was resumed: %+v", obs)
			}
		})
	}
}

// A run of an item the platform marked cancelled is refused for good with the module's own code (never a wait: it can never succeed), while the module holds none of the job's rows; and the
// replay of an item the module HOLDS still answers (the replay wins).
func TestSenders_ARunOfAnItemThePlatformMarkedCancelledIsRefusedForGoodNeverAWaitHandWritten(t *testing.T) {
	for _, module := range senders {
		t.Run(module, func(t *testing.T) {
			rig, _, mod := senderModule(t, module, 1)
			rows := jobenginetestRows(2)
			rig.markCancelled(rows[0].ID)
			_, err := mod.Submit(ctxT(t), jobOf(module, 2), rows)
			expectRefused(t, "a run of a cancelled-marked item", err, "job_item_cancelled", "HTTP 409")
			if got := jobengine.RefusalCode(err); got != "job_item_cancelled" {
				t.Errorf("the refusal's code word is %q, want job_item_cancelled", got)
			}
			if len(rig.held()) != 0 {
				t.Errorf("the module holds %v: a refused run created a job", rig.held())
			}
			// the control: the same rows, nothing marked, are accepted
			rig2, _, mod2 := senderModule(t, module, 1)
			if res, err := mod2.Submit(ctxT(t), jobOf(module, 2), rows); err != nil || len(res.Accepted) != 2 || len(rig2.held()) != 2 {
				t.Fatalf("the control: %+v, %v", res, err)
			}
			// the replay wins: an item the module already holds answers the existing job whatever the marker says now
			rig2.markCancelled(rows[1].ID)
			if res, err := mod2.Submit(ctxT(t), jobOf(module, 2), rows); err != nil || len(res.Accepted) != 2 {
				t.Errorf("the replay of held rows after a marker: %+v, %v; want both accepted", res, err)
			}
			// and the plain conflict is still the wait it was (module 2's back-pressure, module 3's named cause)
			rig3, _, mod3 := senderModule(t, module, 1)
			rig3.busy.Store(1)
			_, err = mod3.Submit(ctxT(t), jobOf(module, 2), rows)
			expectWait(t, "a plain 409 conflict", err, "HTTP 409")
		})
	}
}

// ---- module 1's cancel ----------------------------------------------------------------------------------------------------------------------------------------------------------------

func TestM1Cancel_HandWritten_TheUnclaimedRowsComeBackCancelledAndTheJobCompletes(t *testing.T) {
	rig, _, mod := m1Module(t)
	rows := jobenginetestRows(4)
	j := jobOf(jobs.ModuleVerify, 4)
	res, err := mod.Submit(ctxT(t), j, rows)
	if err != nil {
		t.Fatal(err)
	}
	j.ModuleRef = res.ModuleRef
	rig.settle(rows[0].ID, charge.Outcome{Module: "m1", State: "valid", Reason: "accepted"})
	before := rig.stub.Count()
	if err := mod.Cancel(ctxT(t), j); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	reqs := rig.stub.Requests()[before:]
	if len(reqs) != 1 || reqs[0].Method != "POST" || reqs[0].Path != "/v1/jobs/"+res.ModuleRef+"/cancel" || reqs[0].Auth != "Bearer "+rig.key {
		t.Fatalf("the cancel sent %+v, want ONE POST /v1/jobs/<module 1's id>/cancel with the user's own key", reqs)
	}
	obs, got := observed(t, mod, j)
	if !obs.Complete || obs.Terminal != 4 || obs.Total != 4 {
		t.Errorf("after the cancel: %+v, want complete, 4 final of 4 (settled + cancelled)", obs)
	}
	if len(got) != 4 || got[rows[0].ID].Outcome != (charge.Outcome{Module: "m1", State: "valid", Reason: "accepted"}) {
		t.Fatalf("handed over %d item(s), want the settled row with its verdict and three more", len(got))
	}
	for _, r := range rows[1:] {
		if o := got[r.ID].Outcome; o != (charge.Outcome{Module: "m1", State: "cancelled", Reason: "cancelled_by_caller"}) {
			t.Errorf("the cancelled row %s is %+v, want module 1's own words (the charge table's row M1-24: free)", r.ID, o)
		}
		if d := charge.Decide(got[r.ID].Outcome); d.Kind != charge.Free || d.Rule != "M1-24" {
			t.Errorf("the cancelled row is decided %s by %s, want free by M1-24", d.Kind, d.Rule)
		}
	}
	if d := charge.Decide(got[rows[0].ID].Outcome); d.Kind != charge.Charge {
		t.Errorf("the settled row is decided %s, want a charge (a cancel does not touch what settled)", d.Kind)
	}
	// a repeat is never an error and changes nothing
	if err := mod.Cancel(ctxT(t), j); err != nil {
		t.Errorf("a second cancel: %v", err)
	}
	if obs2, got2 := observed(t, mod, j); !obs2.Complete || len(got2) != 4 {
		t.Errorf("after the second cancel: %+v / %d items", obs2, len(got2))
	}
}

func TestM1Cancel_HandWritten_AJobWithNothingLeftToCancelIsStillAnAnsweredCancelAndNothingIsCancelled(t *testing.T) {
	rig, _, mod := m1Module(t)
	rows := jobenginetestRows(3)
	j := jobOf(jobs.ModuleVerify, 3)
	res, err := mod.Submit(ctxT(t), j, rows)
	if err != nil {
		t.Fatal(err)
	}
	j.ModuleRef = res.ModuleRef
	rig.settleAll(jobenginetestFinals())
	if err := mod.Cancel(ctxT(t), j); err != nil { // already_complete: the module changes nothing, the answer is still a success
		t.Fatalf("cancel of a job whose rows were all final: %v", err)
	}
	obs, got := observed(t, mod, j)
	if !obs.Complete || len(got) != 3 || obs.Detail["cancelled"] != 0 || obs.Detail["cancel_requested"] != false {
		t.Errorf("a cancel with nothing to cancel: %+v / %d items; want complete, three verdicts, nothing cancelled and no cancel recorded at the module", obs, len(got))
	}
	for _, it := range got {
		if it.Outcome.State == "cancelled" {
			t.Errorf("a settled row came back cancelled: %+v", it)
		}
	}
}

func TestM1Cancel_HandWritten_WithoutItsIdOrWhenTheModuleCannotAnswerItWaitsAndSendsNothingItShouldNot(t *testing.T) {
	rig, _, mod := m1Module(t)
	j := jobOf(jobs.ModuleVerify, 2)
	before := rig.stub.Count()
	expectWait(t, "a cancel of a job that carries no module 1 id", mod.Cancel(ctxT(t), j), "module 1 id")
	if rig.stub.Count() != before {
		t.Error("a cancel with no module reference sent a request")
	}
	res, err := mod.Submit(ctxT(t), j, jobenginetestRows(2))
	if err != nil {
		t.Fatal(err)
	}
	j.ModuleRef = res.ModuleRef
	rig.down.Store(true)
	expectWait(t, "a cancel while module 1 is down", mod.Cancel(ctxT(t), j), "cancel")
	rig.down.Store(false)
	if err := mod.Cancel(ctxT(t), j); err != nil { // the control
		t.Errorf("the cancel once module 1 is back: %v", err)
	}
	// a module reference module 1 does not know (404): a wait, never a refusal for good and never ErrNotSupported (the route exists)
	other := j
	other.ModuleRef = "00000000-0000-4000-8000-0000000000ff"
	err = mod.Cancel(ctxT(t), other)
	expectWait(t, "a cancel of a job module 1 does not know", err)
	if errors.Is(err, jobengine.ErrNotSupported) {
		t.Error("a 404 for one job was taken for a module that has no cancel")
	}
}

// Module 1's final rows are settled + cancelled: the status alone answers while nothing is new, the listing is walked when something is, and a complete job is believed only when the listing
// read equals settled + cancelled = submitted.
func TestM1Observe_HandWritten_CancelledRowsAreFinalAndACompleteJobIsBelievedOnlyWhenTheListingAgrees(t *testing.T) {
	rig, _, mod := m1Module(t)
	rows := jobenginetestRows(4)
	j := jobOf(jobs.ModuleVerify, 4)
	res, err := mod.Submit(ctxT(t), j, rows)
	if err != nil {
		t.Fatal(err)
	}
	j.ModuleRef = res.ModuleRef
	rig.settle(rows[0].ID, charge.Outcome{Module: "m1", State: "valid", Reason: "accepted"})
	rig.mu.Lock() // a row module 1 has cancelled while the others are still being worked (the stand-in cancels everything at once; this is the state in between)
	rig.jobs[res.ModuleRef].items[1].cancelled = true
	rig.jobs[res.ModuleRef].cancelRequested = true
	rig.mu.Unlock()

	j.PendingResults = 2 // the engine has both final rows recorded: the status alone answers, the listing is not walked
	before := rig.stub.Count()
	obs, got := observed(t, mod, j)
	if obs.Complete || obs.Terminal != 2 || obs.Total != 4 || len(got) != 0 || rig.stub.Count()-before != 1 {
		t.Errorf("nothing new: %+v, %d item(s), %d request(s); want not complete, 2 final of 4, nothing handed over, one request (the status)", obs, len(got), rig.stub.Count()-before)
	}
	j.PendingResults = 1 // one is not recorded: the cancelled row is news like a verdict is
	before = rig.stub.Count()
	_, got = observed(t, mod, j)
	if len(got) != 2 || got[rows[1].ID].Outcome.State != "cancelled" || rig.stub.Count()-before != 2 {
		t.Errorf("something new: %d item(s) handed over, %d request(s); want the settled and the cancelled row, status then listing", len(got), rig.stub.Count()-before)
	}
	// the rest is cancelled: the job is complete with 4 final rows
	rig.mu.Lock()
	for _, it := range rig.jobs[res.ModuleRef].items {
		if !it.settled {
			it.cancelled = true
		}
	}
	rig.mu.Unlock()
	if obs, got := observed(t, mod, j); !obs.Complete || obs.Terminal != 4 || len(got) != 4 {
		t.Errorf("every row final: %+v / %d items, want complete, 4 of 4", obs, len(got))
	}
	// the control of the believing: a listing that contradicts the totals (it drops its last row) is a wait, never a complete job
	rig.brokenListing.Store(true)
	_, err = mod.Observe(ctxT(t), j, func([]jobs.ResultItem) error { return nil })
	expectWait(t, "a complete job whose listing is short of settled + cancelled", err, "contradicts")
}
