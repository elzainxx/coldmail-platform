package jobengine_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/charge"
	"github.com/coldmail-platform/platform/internal/jobengine"
	"github.com/coldmail-platform/platform/internal/jobs"
)

// Modules 2 and 3 (one implementation, sendapiModule, over the shared sender client) against the stateful stand-in: what Submit holds and answers, what Observe hands over, and what a
// module that cannot answer does to the job. Each test names its paired control.

var senders = []string{jobs.ModuleSendManaged, jobs.ModuleSendBYO}

func idsOf(rows []jobs.EngineRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}

func senderModule(t *testing.T, module string, conc int) (*senderRig, *jobengine.Registry, jobengine.Module) {
	t.Helper()
	rig := newSenderRig(t, module)
	reg := rig.registry(t, conc)
	mod, err := reg.For(ctxT(t), rigUser, module)
	if err != nil {
		t.Fatal(err)
	}
	return rig, reg, mod
}

func expectWait(t *testing.T, what string, err error, contains ...string) {
	t.Helper()
	if !errors.Is(err, jobengine.ErrWait) || errors.Is(err, jobengine.ErrRefused) {
		t.Fatalf("%s: %v, want ErrWait", what, err)
	}
	for _, c := range contains {
		if !strings.Contains(err.Error(), c) {
			t.Errorf("%s: the reason %q does not say %q", what, err.Error(), c)
		}
	}
}

func expectRefused(t *testing.T, what string, err error, contains ...string) {
	t.Helper()
	if !errors.Is(err, jobengine.ErrRefused) || errors.Is(err, jobengine.ErrWait) {
		t.Fatalf("%s: %v, want ErrRefused", what, err)
	}
	for _, c := range contains {
		if !strings.Contains(err.Error(), c) {
			t.Errorf("%s: the reason %q does not say %q", what, err.Error(), c)
		}
	}
}

func TestSender_SubmitRunsEveryRowOnceAndAReplayIsIdempotent(t *testing.T) {
	for _, module := range senders {
		t.Run(module, func(t *testing.T) {
			rig, _, mod := senderModule(t, module, 4)
			rows := jobenginetestRows(12)
			j := jobOf(module, 12)
			res, err := mod.Submit(ctxT(t), j, rows)
			if err != nil || !slices.Equal(res.Accepted, idsOf(rows)) || res.ModuleRef != "" {
				t.Fatalf("Submit = %+v, %v; want all 12 rows in order and no module reference", res, err)
			}
			if got := rig.held(); len(got) != 12 || rig.runs.Load() != 12 {
				t.Fatalf("the module holds %d item(s) after %d run call(s), want 12 and 12", len(got), rig.runs.Load())
			}
			// the replay: the module answers 200 for each item it holds; nothing is created twice and every row is accepted again
			res, err = mod.Submit(ctxT(t), j, rows)
			if err != nil || !slices.Equal(res.Accepted, idsOf(rows)) || len(rig.held()) != 12 || rig.runs.Load() != 24 {
				t.Fatalf("the replay = %+v, %v; the module holds %d, run calls %d; want the same 12 rows, 12 held, 24 calls", res, err, len(rig.held()), rig.runs.Load())
			}
			// every call carried the tenant of the job, in the body (sendapi's own check of tenancy)
			for _, r := range rig.stub.Requests() {
				if !strings.Contains(string(r.Body), rigUser) {
					t.Fatalf("a run call carried no tenant: %s", r.Body)
				}
			}
		})
	}
}

func TestSender_TheFirstRowGoesAloneAndTheRestRunWithBoundedConcurrency(t *testing.T) {
	for _, conc := range []int{1, 3} {
		t.Run(fmt.Sprintf("concurrency %d", conc), func(t *testing.T) {
			rig, _, mod := senderModule(t, jobs.ModuleSendManaged, conc)
			rig.delay = 25 * time.Millisecond // a call takes long enough for the others to be in flight with it
			rows := jobenginetestRows(10)
			if res, err := mod.Submit(ctxT(t), jobOf(jobs.ModuleSendManaged, 10), rows); err != nil || len(res.Accepted) != 10 {
				t.Fatalf("Submit = %+v, %v", res, err)
			}
			if got := rig.maxIn.Load(); got != int64(conc) {
				t.Errorf("at most %d run calls were in flight together, want exactly %d (the dial PLATFORM_JOB_SUBMIT_CONCURRENCY, reached and never passed)", got, conc)
			}
			ev := rig.eventLog()
			if len(ev) < 3 || ev[0] != "start" || ev[1] != "end" {
				t.Errorf("the first row did not go alone: the events begin %v, want start then end before the next start", ev[:min(4, len(ev))])
			}
		})
	}
}

func TestSender_APartialAcceptanceIsAnAnswerAndTheEngineCanContinue(t *testing.T) {
	for _, module := range senders {
		for _, conc := range []int{1, 3} {
			t.Run(fmt.Sprintf("%s concurrency %d", module, conc), func(t *testing.T) {
				rig, _, mod := senderModule(t, module, conc)
				rig.capacity.Store(4) // the module takes four items and then answers 409
				rows := jobenginetestRows(12)
				res, err := mod.Submit(ctxT(t), jobOf(module, 12), rows)
				if err != nil {
					t.Fatalf("a batch stopped by a 409 after four rows answered %v: the rows the module holds must be handed back (the engine drops an answer that comes with an error)", err)
				}
				if conc == 1 && !slices.Equal(res.Accepted, idsOf(rows[:4])) {
					t.Errorf("accepted %v, want exactly the first four rows", res.Accepted)
				}
				held := rig.held()
				if len(res.Accepted) != 4 || len(held) != 4 {
					t.Fatalf("accepted %d row(s), the module holds %d: want 4 and 4", len(res.Accepted), len(held))
				}
				for _, id := range res.Accepted {
					if !slices.Contains(held, id) {
						t.Errorf("the row %s was handed back as accepted and the module does not hold it", id)
					}
				}
				if got := rig.runs.Load(); got >= 12 {
					t.Errorf("%d run call(s) for 12 rows: the batch was not stopped by the 409", got)
				}
				// the engine continues: the next tick offers the rest (the module still full: nothing accepted, the answer is ErrWait), then room is made and it takes them
				rest := slices.DeleteFunc(slices.Clone(rows), func(r jobs.EngineRow) bool { return slices.Contains(res.Accepted, r.ID) }) // what PendingRows would offer again
				if conc == 1 {
					res2, err := mod.Submit(ctxT(t), jobOf(module, 12), rest)
					expectWait(t, "the module is still full", err, "409")
					if len(res2.Accepted) != 0 {
						t.Errorf("a full module accepted %d row(s)", len(res2.Accepted))
					}
				}
				rig.capacity.Store(0)
				res3, err := mod.Submit(ctxT(t), jobOf(module, 12), rest)
				if err != nil || len(res3.Accepted) != len(rest) || len(rig.held()) != 12 {
					t.Fatalf("after room was made: %+v, %v, the module holds %d; want the other %d rows and 12 held", res3, err, len(rig.held()), len(rest))
				}
			})
		}
	}
}

func TestSender_ABusyModuleWithNothingAcceptedIsErrWaitAndModuleThreeNamesItsCauses(t *testing.T) {
	for _, module := range senders {
		t.Run(module, func(t *testing.T) {
			rig, _, mod := senderModule(t, module, 4)
			rig.busy.Store(1000)
			rig.conflict = []string{"queue_cap", "campaign_active"}
			res, err := mod.Submit(ctxT(t), jobOf(module, 5), jobenginetestRows(5))
			expectWait(t, "a 409 on the first row", err, "409")
			if module == jobs.ModuleSendBYO {
				for _, cause := range []string{"queue_cap", "campaign_active"} {
					if !strings.Contains(err.Error(), cause) {
						t.Errorf("module 3's reason %q does not name the cause %q the module gave", err.Error(), cause)
					}
				}
			} else if strings.Contains(err.Error(), "causes") {
				t.Errorf("module 2's 409 has no causes and the reason invents some: %v", err)
			}
			if len(res.Accepted) != 0 || len(rig.held()) != 0 || rig.runs.Load() != 1 {
				t.Errorf("accepted %d, held %d, run calls %d: the first row alone met the 409, nothing else was tried", len(res.Accepted), len(rig.held()), rig.runs.Load())
			}
			// the control: the same module, not busy, takes the rows
			rig.busy.Store(0)
			if res, err := mod.Submit(ctxT(t), jobOf(module, 5), jobenginetestRows(5)); err != nil || len(res.Accepted) != 5 {
				t.Errorf("the control: %+v, %v", res, err)
			}
		})
	}
}

func TestSender_ARefusalForGoodIsErrRefusedOnlyWhenNothingOfTheJobIsHeld(t *testing.T) {
	for _, module := range senders {
		t.Run(module, func(t *testing.T) {
			rig, _, mod := senderModule(t, module, 2)
			rows := jobenginetestRows(4)
			for status, word := range map[int]string{400: "validation_failed", 404: "not_found"} {
				rig.failRun.Store(int64(status))
				res, err := mod.Submit(ctxT(t), jobOf(module, 4), rows)
				expectRefused(t, fmt.Sprintf("HTTP %d with nothing held", status), err, word)
				if len(res.Accepted) != 0 {
					t.Errorf("a refused job accepted %d row(s)", len(res.Accepted))
				}
				// the reason is the module's code word and never its text, an id, an address or a key
				for _, bad := range []string{"@", rigUser, rig.token, "message"} {
					if strings.Contains(err.Error(), bad) {
						t.Errorf("the reason %q carries %q", err.Error(), bad)
					}
				}
			}
			// with rows of the job already held at the module, failing the job would leave them running at the module and charged to nobody: it waits
			j := jobOf(module, 4)
			j.Submitted = 2
			rig.failRun.Store(400)
			_, err := mod.Submit(ctxT(t), j, rows)
			expectWait(t, "a refusal with rows already held", err, "validation_failed", "already held")
			// the credential of the platform is refused: that is the platform's configuration, never the customer's job
			for _, status := range []int{401, 403} {
				rig.failRun.Store(int64(status))
				_, err := mod.Submit(ctxT(t), jobOf(module, 4), rows)
				expectWait(t, fmt.Sprintf("HTTP %d: the module refuses our credential", status), err, "credential", "unauthenticated")
			}
			// the control: with no fault the same rows are accepted
			rig.failRun.Store(0)
			if res, err := mod.Submit(ctxT(t), jobOf(module, 4), rows); err != nil || len(res.Accepted) != 4 {
				t.Errorf("the control: %+v, %v", res, err)
			}
		})
	}
}

func TestSender_AModuleThatCannotAnswerLeavesTheJobWhereItIsAndIsCounted(t *testing.T) {
	for _, module := range senders {
		t.Run(module, func(t *testing.T) {
			rig, reg, mod := senderModule(t, module, 2)
			rows := jobenginetestRows(3)
			rig.down.Store(true) // 503 unavailable
			if res, err := mod.Submit(ctxT(t), jobOf(module, 3), rows); len(res.Accepted) != 0 {
				t.Errorf("a module that is down accepted rows: %+v", res)
			} else {
				expectWait(t, "503 unavailable", err, "unavailable", "HTTP 503")
			}
			_, err := mod.Observe(ctxT(t), jobOf(module, 3), func([]jobs.ResultItem) error { return nil })
			expectWait(t, "observe against 503", err)
			if len(rig.held()) != 0 {
				t.Errorf("an outage created %d item(s)", len(rig.held()))
			}
			// connection refused: the module's server is gone
			rig.down.Store(false)
			rig.stub.Close()
			_, err = mod.Submit(ctxT(t), jobOf(module, 3), rows)
			expectWait(t, "connection refused", err, "unavailable")
			if reg.UnexpectedAnswers() != 0 {
				t.Errorf("an outage was counted as an answer the platform does not understand: %d", reg.UnexpectedAnswers())
			}
		})
	}
}

func TestSender_AnAnswerThePlatformDoesNotUnderstandWaitsAndIsCounted(t *testing.T) {
	for _, module := range senders {
		t.Run(module, func(t *testing.T) {
			rig, reg, mod := senderModule(t, module, 1)
			// an internal error of the module (500) and a 200 with a body that is not the contract: both wait, both are counted, neither fails the job
			rig.failRun.Store(500)
			_, err := mod.Submit(ctxT(t), jobOf(module, 2), jobenginetestRows(2))
			expectWait(t, "HTTP 500", err, "internal", "does not understand")
			if reg.UnexpectedAnswers() != 1 {
				t.Errorf("a module fault was counted %d time(s), want 1", reg.UnexpectedAnswers())
			}
			// the control: a plain refusal (400) is an answer the platform understands, and is not counted
			rig.failRun.Store(400)
			_, _ = mod.Submit(ctxT(t), jobOf(module, 2), jobenginetestRows(2))
			if reg.UnexpectedAnswers() != 1 {
				t.Errorf("a refusal was counted as an answer the platform does not understand: %d", reg.UnexpectedAnswers())
			}
		})
	}
}

func TestSender_ObserveHandsOverTerminalItemsOnlyWithTheModulesOwnWords(t *testing.T) {
	for _, module := range senders {
		t.Run(module, func(t *testing.T) {
			rig, _, mod := senderModule(t, module, 4)
			rows := jobenginetestRows(8)
			if _, err := mod.Submit(ctxT(t), jobOf(module, 8), rows); err != nil {
				t.Fatal(err)
			}
			want := map[string]charge.Outcome{}
			for i, o := range []charge.Outcome{
				{Module: module, State: "sent"}, {Module: module, State: "bounced", Reason: "bounce_permanent"}, {Module: module, State: "failed", Reason: "attempts_exhausted"},
			} {
				if module == jobs.ModuleSendBYO {
					o.Reason = "" // module 3 has no reason key: its outcomes carry none
				}
				rig.settle(rows[i].ID, o)
				want[rows[i].ID] = o
			}
			rig.settle(rows[3].ID, charge.Outcome{Module: module, State: "sending"})  // in flight: not final
			rig.settle(rows[4].ID, charge.Outcome{Module: module, State: "assigned"}) // claimed: not final
			got := map[string]jobs.ResultItem{}
			obs, err := mod.Observe(ctxT(t), jobOf(module, 8), func(items []jobs.ResultItem) error {
				for _, it := range items {
					got[it.ItemRef] = it
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 3 {
				t.Fatalf("%d item(s) handed over, want the 3 final ones (not the two in flight, not the three queued): %v", len(got), got)
			}
			for ref, o := range want {
				if got[ref].Outcome != o {
					t.Errorf("the item %s was handed over as %+v, the module reported %+v", ref, got[ref].Outcome, o)
				}
				// the detail holds words and counts, never an address, a message id or an account
				for k := range got[ref].Detail {
					if !slices.Contains([]string{"attempts", "error_class", "completed_at"}, k) {
						t.Errorf("the detail of an item carries %q", k)
					}
				}
			}
			if obs.Total != 8 || obs.Terminal != 3 || obs.Complete {
				t.Errorf("the observation = %+v, want total 8, terminal 3, not complete", obs)
			}
		})
	}
}

func TestSender_ObserveWalksEveryPageOnlyWhenThereIsSomethingNewAndAnUnknownCampaignIsEmpty(t *testing.T) {
	rig, _, mod := senderModule(t, jobs.ModuleSendManaged, 8)
	rows := jobenginetestRows(450) // three pages of the module's 200
	if res, err := mod.Submit(ctxT(t), jobOf(jobs.ModuleSendManaged, 450), rows); err != nil || len(res.Accepted) != 450 {
		t.Fatalf("Submit = %d row(s), %v", len(res.Accepted), err)
	}
	for _, r := range rows[:300] {
		rig.settle(r.ID, charge.Outcome{Module: "m2", State: "sent"})
	}
	count := func(j jobs.EngineJob) (items, requests int, obs jobengine.Observation) {
		before := rig.stub.Count()
		var err error
		obs, err = mod.Observe(ctxT(t), j, func(it []jobs.ResultItem) error { items += len(it); return nil })
		if err != nil {
			t.Fatal(err)
		}
		return items, rig.stub.Count() - before, obs
	}
	j := jobOf(jobs.ModuleSendManaged, 450)
	if items, reqs, obs := count(j); items != 300 || reqs != 3 || obs.Terminal != 300 {
		t.Fatalf("a first observation: %d item(s) in %d request(s), terminal %d; want 300 items over the 3 pages", items, reqs, obs.Terminal)
	}
	// the engine recorded all 300: nothing is new, so ONE request (the first page's totals) answers, and nothing is handed over
	j.PendingResults = 300
	if items, reqs, obs := count(j); items != 0 || reqs != 1 || obs.Terminal != 300 || obs.Total != 450 {
		t.Fatalf("an observation with nothing new: %d item(s) in %d request(s), %+v; want 0 items and exactly one request", items, reqs, obs)
	}
	// one more settles: the walk is made again
	rig.settle(rows[300].ID, charge.Outcome{Module: "m2", State: "bounced", Reason: "bounce_permanent"})
	if items, reqs, obs := count(j); items != 301 || reqs != 3 || obs.Terminal != 301 {
		t.Fatalf("after one more settled: %d item(s) in %d request(s), terminal %d; want 301 over 3 requests", items, reqs, obs.Terminal)
	}

	// a campaign the module knows nothing of answers an empty page: nothing, not complete, no error
	other := newSenderRig(t, jobs.ModuleSendBYO)
	reg := other.registry(t, 1)
	m3mod, err := reg.For(ctxT(t), rigUser, jobs.ModuleSendBYO)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	obs, err := m3mod.Observe(ctxT(t), jobOf(jobs.ModuleSendBYO, 5), func(it []jobs.ResultItem) error { n += len(it); return nil })
	if err != nil || n != 0 || obs.Complete || obs.Total != 0 || obs.Terminal != 0 {
		t.Errorf("an unknown campaign: %d item(s), %+v, %v; want nothing, not complete, no error", n, obs, err)
	}
}

func TestSender_ACompleteCampaignWhoseListingContradictsItsProgressIsNeverBelieved(t *testing.T) {
	for _, module := range senders {
		t.Run(module, func(t *testing.T) {
			rig, _, mod := senderModule(t, module, 2)
			rows := jobenginetestRows(5)
			if _, err := mod.Submit(ctxT(t), jobOf(module, 5), rows); err != nil {
				t.Fatal(err)
			}
			for _, r := range rows {
				rig.settle(r.ID, charge.Outcome{Module: module, State: "sent"})
			}
			// the control: the honest module is complete
			if obs, err := mod.Observe(ctxT(t), jobOf(module, 5), func([]jobs.ResultItem) error { return nil }); err != nil || !obs.Complete {
				t.Fatalf("the control: %+v, %v", obs, err)
			}
			// the module's progress says all five are terminal and its listing holds four
			rig.dropListing.Store(1)
			read := 0
			_, err := mod.Observe(ctxT(t), jobOf(module, 5), func(it []jobs.ResultItem) error { read += len(it); return nil })
			expectWait(t, "a listing that contradicts the totals", err, "contradicts")
			if read != 4 {
				t.Errorf("%d item(s) were handed over before the contradiction was seen, want the 4 the listing holds", read)
			}
		})
	}
}

func TestSender_CancelIsIdempotentAndAnOutageIsErrWait(t *testing.T) {
	for _, module := range senders {
		t.Run(module, func(t *testing.T) {
			rig, _, mod := senderModule(t, module, 2)
			rows := jobenginetestRows(4)
			if _, err := mod.Submit(ctxT(t), jobOf(module, 4), rows); err != nil {
				t.Fatal(err)
			}
			rig.settle(rows[0].ID, charge.Outcome{Module: module, State: "sent"})
			for i := 0; i < 2; i++ {
				if err := mod.Cancel(ctxT(t), jobOf(module, 4)); err != nil {
					t.Fatalf("cancel #%d: %v", i+1, err)
				}
			}
			if rig.state(rows[0].ID) != "sent" || rig.state(rows[1].ID) != "cancelled" || rig.state(rows[3].ID) != "cancelled" {
				t.Errorf("after the cancel: %s %s %s, want the settled item left alone and the queued ones cancelled", rig.state(rows[0].ID), rig.state(rows[1].ID), rig.state(rows[3].ID))
			}
			rig.down.Store(true)
			expectWait(t, "cancel while the module is down", mod.Cancel(ctxT(t), jobOf(module, 4)), "unavailable")
		})
	}
}

func TestSender_TheEnginesOwnErrorsAreNeverTakenForTheModulesOrMapped(t *testing.T) {
	rig, _, mod := senderModule(t, jobs.ModuleSendManaged, 2)
	rows := jobenginetestRows(2)
	if _, err := mod.Submit(ctxT(t), jobOf(jobs.ModuleSendManaged, 2), rows); err != nil {
		t.Fatal(err)
	}
	rig.settle(rows[0].ID, charge.Outcome{Module: "m2", State: "sent"})
	boom := errors.New("the store said no")
	_, err := mod.Observe(ctxT(t), jobOf(jobs.ModuleSendManaged, 2), func([]jobs.ResultItem) error { return boom })
	if err != boom { //nolint:errorlint // identity is the property: the very error, not a wrapper of it
		t.Errorf("the callback's error came back as %v (%T), want the same error value", err, err)
	}
	// and a context that ended is no answer of the module either: it waits, and says so
	ctx, cancel := context.WithCancel(ctxT(t))
	cancel()
	_, err = mod.Observe(ctx, jobOf(jobs.ModuleSendManaged, 2), func([]jobs.ResultItem) error { return nil })
	expectWait(t, "a cancelled context", err)
}

// jobenginetestRows are the rows of a job: uuids and addresses of the reserved example.test domain.
func jobenginetestRows(n int) []jobs.EngineRow {
	out := make([]jobs.EngineRow, n)
	for i := range out {
		out[i] = jobs.EngineRow{ID: fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1), Ordinal: i + 1, EmailNorm: fmt.Sprintf("row%d@example.test", i+1)}
	}
	return out
}
