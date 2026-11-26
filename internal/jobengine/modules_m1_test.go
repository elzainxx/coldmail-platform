package jobengine_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/charge"
	"github.com/coldmail-platform/platform/internal/jobengine"
	"github.com/coldmail-platform/platform/internal/jobs"
)

// Module 1 (m1Module over the m1 adapter) against the stateful stand-in: the key, the whole-list rule, the replay, the refusals and every way the results can disagree with the totals.

func m1Module(t *testing.T) (*m1Rig, *jobengine.Registry, jobengine.Module) {
	t.Helper()
	rig := newM1Rig(t)
	reg := rig.registry(t, &rigScoper{key: rig.key}, nil)
	mod, err := reg.For(ctxT(t), rigUser, jobs.ModuleVerify)
	if err != nil {
		t.Fatal(err)
	}
	return rig, reg, mod
}

func collect(into map[string]jobs.ResultItem) func([]jobs.ResultItem) error {
	return func(items []jobs.ResultItem) error {
		for _, it := range items {
			into[it.ItemRef] = it
		}
		return nil
	}
}

func TestM1_SubmitSendsTheWholeListUnderTheJobIdAndAReplayIsTheSameModuleJob(t *testing.T) {
	rig, _, mod := m1Module(t)
	rows := jobenginetestRows(6)
	j := jobOf(jobs.ModuleVerify, 6)
	res, err := mod.Submit(ctxT(t), j, rows)
	if err != nil || !slices.Equal(res.Accepted, idsOf(rows)) || res.ModuleRef == "" {
		t.Fatalf("Submit = %+v, %v; want every row, in order, and module 1's id of the job", res, err)
	}
	// the idempotency key is the platform job id, and the body is the rows: item_id the row id, address the normalised address
	if keys := rig.submittedKeys(); len(keys) != 1 || keys[0] != rigCampaign {
		t.Fatalf("the Idempotency-Key was %v, want the job id %s", keys, rigCampaign)
	}
	var body struct {
		Items []struct {
			ItemID  string `json:"item_id"`
			Address string `json:"address"`
		} `json:"items"`
	}
	req := rig.stub.Requests()[0]
	if err := json.Unmarshal(req.Body, &body); err != nil || len(body.Items) != 6 {
		t.Fatalf("the request body %s: %v", req.Body, err)
	}
	for i, it := range body.Items {
		if !strings.EqualFold(it.ItemID, rows[i].ID) || it.Address != rows[i].EmailNorm {
			t.Errorf("row %d went as item_id %q address %q, want the row id and its normalised address", i, it.ItemID, it.Address)
		}
	}
	if req.Auth != "Bearer "+rig.key {
		t.Errorf("module 1 was called with %q, want the user's own key", req.Auth)
	}
	// a replay is the same module job (a crash between the module's answer and the engine's record is repaired by offering the same rows again)
	res2, err := mod.Submit(ctxT(t), j, rows)
	if err != nil || res2.ModuleRef != res.ModuleRef || rig.jobCount() != 1 {
		t.Fatalf("the replay = %+v, %v; the module holds %d job(s); want the same reference and one job", res2, err, rig.jobCount())
	}
	if keys := rig.submittedKeys(); len(keys) != 2 || keys[1] != rigCampaign {
		t.Errorf("the replay's key was %v", keys)
	}
}

func TestM1_AListThatDoesNotFitInOneCallWaitsAndSendsNothing(t *testing.T) {
	rig, _, mod := m1Module(t)
	rows := jobenginetestRows(6)
	// the engine offers 6 of the job's 10 rows (its batch dial is below the list), or rows after the first were already handed over: module 1 would take a second request under the same key as the
	// 409 of a different request, so nothing is sent and the job waits (free, until its start deadline)
	j := jobOf(jobs.ModuleVerify, 10)
	_, err := mod.Submit(ctxT(t), j, rows)
	expectWait(t, "a partial list", err, "ONE request", "6 of the job's 10")
	j = jobOf(jobs.ModuleVerify, 6)
	j.Submitted = 3
	_, err = mod.Submit(ctxT(t), j, rows)
	expectWait(t, "rows already handed over", err, "3 already handed over")
	if rig.stub.Count() != 0 || rig.jobCount() != 0 {
		t.Errorf("%d request(s) and %d module job(s): nothing may be sent for a list that is not the whole job", rig.stub.Count(), rig.jobCount())
	}
	// the control: the whole list is sent
	if res, err := mod.Submit(ctxT(t), jobOf(jobs.ModuleVerify, 6), rows); err != nil || len(res.Accepted) != 6 {
		t.Errorf("the control: %+v, %v", res, err)
	}
}

func TestM1_APendingRowTheAdaptersRuleCallsMalformedSendsNothing(t *testing.T) {
	rig, _, mod := m1Module(t)
	rows := jobenginetestRows(3)
	rows[1].EmailNorm = "no-at-sign.example.test" // the store called it good; module 1's rule does not
	_, err := mod.Submit(ctxT(t), jobOf(jobs.ModuleVerify, 3), rows)
	expectWait(t, "a malformed pending row", err, "malformed", "nothing is sent")
	if rig.stub.Count() != 0 {
		t.Errorf("%d request(s): a list with a malformed row must not be sent in part", rig.stub.Count())
	}
	if strings.Contains(err.Error(), "no-at-sign") {
		t.Errorf("the reason carries the address: %v", err)
	}
}

func TestM1_TheSameKeyForAnotherRequestIsTheOneRefusalForGood(t *testing.T) {
	rig, _, mod := m1Module(t)
	rows := jobenginetestRows(4)
	if _, err := mod.Submit(ctxT(t), jobOf(jobs.ModuleVerify, 4), rows); err != nil {
		t.Fatal(err)
	}
	// a different list under the same job id can only be a platform bug
	_, err := mod.Submit(ctxT(t), jobOf(jobs.ModuleVerify, 3), rows[:3])
	expectRefused(t, "the idempotency key reused", err, "idempotency key", "platform bug")
	if rig.jobCount() != 1 {
		t.Errorf("the module holds %d jobs, want the one", rig.jobCount())
	}
}

func TestM1_ACountThatIsNotThePlansAndAPausedOrDownModuleAreErrWaitNeverInvented(t *testing.T) {
	rig, reg, mod := m1Module(t)
	rows := jobenginetestRows(3)
	rig.miscount.Store(true)
	res, err := mod.Submit(ctxT(t), jobOf(jobs.ModuleVerify, 3), rows)
	expectWait(t, "a 202 whose counts are not the plan's", err, "counts.total")
	if len(res.Accepted) != 0 || res.ModuleRef != "" {
		t.Errorf("a job whose counts disagree was accepted: %+v", res)
	}
	if reg.UnexpectedAnswers() != 1 {
		t.Errorf("the disagreement was counted %d time(s), want 1", reg.UnexpectedAnswers())
	}
	// module 1's kill switch (503 paused) and an outage wait as well
	rig2, _, mod2 := m1Module(t)
	rig2.paused.Store(true)
	_, err = mod2.Submit(ctxT(t), jobOf(jobs.ModuleVerify, 3), rows)
	expectWait(t, "module 1 paused", err, "paused")
	rig2.paused.Store(false)
	rig2.down.Store(true)
	_, err = mod2.Submit(ctxT(t), jobOf(jobs.ModuleVerify, 3), rows)
	expectWait(t, "module 1 down", err, "unavailable")
	if rig2.jobCount() != 0 {
		t.Errorf("an outage created %d job(s)", rig2.jobCount())
	}
	// the control: a healthy module takes the list
	rig2.down.Store(false)
	if res, err := mod2.Submit(ctxT(t), jobOf(jobs.ModuleVerify, 3), rows); err != nil || len(res.Accepted) != 3 {
		t.Errorf("the control: %+v, %v", res, err)
	}
}

func TestM1_ObserveWalksEveryPageOfAJobOfThousandsAndOnlyWhenThereIsSomethingNew(t *testing.T) {
	rig, _, mod := m1Module(t)
	const n = 2500
	rows := jobenginetestRows(n)
	j := jobOf(jobs.ModuleVerify, n)
	res, err := mod.Submit(ctxT(t), j, rows)
	if err != nil {
		t.Fatal(err)
	}
	j.ModuleRef = res.ModuleRef // what the engine's Start recorded and the store hands back with the job
	finals := jobenginetestFinals()
	rig.settleAll(finals)
	got := map[string]jobs.ResultItem{}
	before := rig.stub.Count()
	obs, err := mod.Observe(ctxT(t), j, collect(got))
	if err != nil {
		t.Fatal(err)
	}
	if requests := rig.stub.Count() - before; requests != 4 { // the status, then three pages of 1,000
		t.Errorf("%d request(s) to read a job of %d rows, want 4: the status and three pages of 1,000", requests, n)
	}
	if len(got) != n || !obs.Complete || obs.Total != n || obs.Terminal != n {
		t.Fatalf("handed over %d item(s), observation %+v; want all %d and complete", len(got), obs, n)
	}
	for i, r := range rows {
		it, ok := got[r.ID]
		if !ok || it.Outcome != finals[i%len(finals)] || it.Detail["ordinal"] != i {
			t.Fatalf("row %d: %+v (present %v), want the outcome %+v at ordinal %d", i, it, ok, finals[i%len(finals)], i)
		}
	}
	// the engine recorded them all and the job is not complete in this paired case: nothing is new, so ONE request (the status) answers
	rig2, _, mod2 := m1Module(t)
	res4, err := mod2.Submit(ctxT(t), jobOf(jobs.ModuleVerify, 4), jobenginetestRows(4))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range jobenginetestRows(4)[:3] {
		rig2.settle(r.ID, finals[0])
	}
	j4 := jobOf(jobs.ModuleVerify, 4)
	j4.ModuleRef = res4.ModuleRef
	j4.PendingResults = 3
	before = rig2.stub.Count()
	calls := 0
	obs, err = mod2.Observe(ctxT(t), j4, func([]jobs.ResultItem) error { calls++; return nil })
	if err != nil || calls != 0 || rig2.stub.Count()-before != 1 || obs.Complete || obs.Terminal != 3 {
		t.Errorf("nothing new: %d callback(s), %d request(s), %+v, %v; want none, one (the status), 3 terminal, not complete", calls, rig2.stub.Count()-before, obs, err)
	}
	// the control: the same job with the engine having recorded fewer walks the listing
	j4.PendingResults = 2
	calls = 0
	if _, err := mod2.Observe(ctxT(t), j4, func(it []jobs.ResultItem) error { calls += len(it); return nil }); err != nil || calls != 3 {
		t.Errorf("something new: %d item(s) handed over, %v; want the 3 settled rows", calls, err)
	}
}

func jobenginetestFinals() []charge.Outcome {
	return []charge.Outcome{
		{Module: "m1", State: "valid", Reason: "accepted"}, {Module: "m1", State: "invalid", Reason: "no_mx"}, {Module: "m1", State: "risky", Reason: "catch_all"},
		{Module: "m1", State: "unknown", Reason: "unknown_error", FailureOrigin: "own"},
	}
}

func TestM1_ARepeatOfAnAddressIsNeverHandedOverAndAListingThatContradictsTheTotalsIsNeverComplete(t *testing.T) {
	rig, reg, mod := m1Module(t)
	rows := jobenginetestRows(3)
	j := jobOf(jobs.ModuleVerify, 3)
	res, err := mod.Submit(ctxT(t), j, rows)
	if err != nil {
		t.Fatal(err)
	}
	j.ModuleRef = res.ModuleRef
	rig.settleAll(jobenginetestFinals())
	rig.markDuplicate(rows[2].ID, 0) // module 1 says row 3 repeats row 1: the platform's own duplicate, never module 1's verdict (it would be charged twice)
	got := map[string]jobs.ResultItem{}
	obs, err := mod.Observe(ctxT(t), j, collect(got))
	if err != nil || len(got) != 2 || got[rows[2].ID].ItemRef != "" || !obs.Complete || obs.Detail["skipped_duplicates"] != 1 {
		t.Fatalf("a repeat: %d item(s) handed over, %+v, %v; want the two others, the repeat counted and left to the engine's completeness check", len(got), obs, err)
	}
	// a complete job whose listing holds fewer rows than its totals say settled: ErrWait, the rows read stay handed over, and the module is not believed
	rig.clearDuplicates()
	rig.brokenListing.Store(true)
	got = map[string]jobs.ResultItem{}
	_, err = mod.Observe(ctxT(t), j, collect(got))
	expectWait(t, "the listing contradicts the totals", err, "contradicts the totals")
	if len(got) != 2 {
		t.Errorf("%d item(s) handed over before the contradiction was seen, want the 2 the listing holds", len(got))
	}
	if reg.UnexpectedAnswers() < 1 {
		t.Errorf("the contradiction was not counted")
	}
	// the control: the honest listing completes
	rig.brokenListing.Store(false)
	got = map[string]jobs.ResultItem{}
	if obs, err := mod.Observe(ctxT(t), j, collect(got)); err != nil || !obs.Complete || len(got) != 3 {
		t.Errorf("the control: %d item(s), %+v, %v", len(got), obs, err)
	}
}

// Module 1 CAN be cancelled since the close-out (TestM1Cancel_*), so the test that said it could not is now about the callback's error and about what module 1 really has no route for: a pause and a resume.
func TestM1_TheCallbacksErrorComesBackAsItWas_AndPauseAndResumeAreNotSupported(t *testing.T) {
	rig, _, mod := m1Module(t)
	rows := jobenginetestRows(2)
	res, err := mod.Submit(ctxT(t), jobOf(jobs.ModuleVerify, 2), rows)
	if err != nil {
		t.Fatal(err)
	}
	rig.settleAll(jobenginetestFinals())
	boom := errors.New("the store said no")
	j2 := jobOf(jobs.ModuleVerify, 2)
	j2.ModuleRef = res.ModuleRef
	_, err = mod.Observe(ctxT(t), j2, func([]jobs.ResultItem) error { return boom })
	if err != boom { //nolint:errorlint // identity is the property
		t.Errorf("the callback's error came back as %v (%T), want the same error value", err, err)
	}
	before := rig.stub.Count()
	for name, call := range map[string]func(context.Context, jobs.EngineJob) error{"pause": mod.Pause, "resume": mod.Resume} {
		if err := call(context.Background(), j2); !errors.Is(err, jobengine.ErrNotSupported) || errors.Is(err, jobengine.ErrWait) {
			t.Errorf("%s = %v, want ErrNotSupported (module 1 has no per-job pause)", name, err)
		}
	}
	if rig.stub.Count() != before {
		t.Errorf("a pause or a resume that cannot be made sent a request")
	}
}
