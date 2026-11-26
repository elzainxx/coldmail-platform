//go:build harness

package harness

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/charge"
	"github.com/coldmail-platform/platform/internal/jobs"
)

// Phase 4 chunks B7 to B9, the engine's half of the job store (internal/jobs engine.go) against a REAL Postgres, as the platform_app login the service runs as. Every test builds its jobs through the
// real store (CreateDraft, Preview, Confirm), except where it needs a state the store never produces (an administrator ending a job by hand, a deadline in the past): those use the fixtures of
// jobs_helpers_test.go. Every test starts with module 1's vendor unit OFF (newJeEnv) and has a paired control that fails when the property it proves breaks. Helpers: jobs_engine_helpers_test.go.
//
//	StartRefusesAScheduledJobBeforeItsTimeAndAcceptsItAfter     the database's clock decides; the refusal is typed, writes nothing; the work list agrees
//	StartIsIdempotent...                                        a replay is the same job; the module's reference is unique per module; the lifecycle's refusals arrive typed
//	RecordResults...                                            the settlement and the results in one transaction: the ledger, the results and the report agree; a replay writes nothing
//	Complete / Finish / Expire / FailJob / Release              the end of a job: one transaction, the remainder released once, the platform's own results for rows no module answered
//	EngineWork...                                               which jobs the engine must look at, with their counts, oldest progress first
//	EnsureModuleItems / MarkSubmitted / PendingRows             the rows handed to modules 2 and 3, idempotent
//	EveryStepTakesTheUserModuleLockFirst                        a call is PROVEN blocked behind the (user, module) advisory lock

func TestJobsEngine_StartRefusesAScheduledJobBeforeItsTimeAndAcceptsItAfter(t *testing.T) {
	e := newJeEnv(t)
	ctx := testCtx(t)
	u := e.newUser()
	at := time.Now().Add(time.Hour)
	j := e.confirmRows(u, "m2", jeAddrs(3, "s"), jeConfirmOpts{Start: &at})
	if j.Status != jobs.StatusScheduled {
		t.Fatalf("the fixture job is %s, want scheduled", j.Status)
	}
	if _, listed := e.workOf(j.ID); listed {
		t.Fatal("a scheduled job whose time has not come is engine work")
	}
	before := e.fingerprint()
	_, err := e.jobs.Start(ctx, j.ID, "")
	jeNotAllowed(t, "Start of a scheduled job before its time", err, "job_started_early")
	if got := e.jobStatus(j.ID); got != jobs.StatusScheduled {
		t.Fatalf("a refused Start left the job %s", got)
	}
	if e.fingerprint() != before {
		t.Fatal("a refused Start wrote something")
	}

	// the control: time passes (the start time is one second ago), and the same call goes through
	jobMakeDue(t, e.admin, j.ID)
	w, listed := e.workOf(j.ID)
	if !listed || w.Status != jobs.StatusScheduled || w.DeadlinePassed {
		t.Fatalf("a scheduled job whose time has come must be engine work (listed %v, %+v)", listed, w.Job.Status)
	}
	got, err := e.jobs.Start(ctx, j.ID, "")
	if err != nil || got.Status != jobs.StatusRunning || got.StartedAt == nil {
		t.Fatalf("Start after the scheduled time: %+v, %v", got, err)
	}
	if who := e.str(`SELECT actor_kind || '|' || COALESCE(acting_user_id::text, '-') || '|' || COALESCE(acting_admin_id::text, '-') FROM platform.job_events
		WHERE job_id = $1 AND kind = 'transition' AND to_status = 'running'`, j.ID); who != "engine|-|-" {
		t.Errorf("the move to running was written by %q, want the engine with no actor id", who)
	}
}

func TestJobsEngine_StartIsIdempotentRecordsTheModuleReferenceAndRefusesWhatTheLifecycleRefuses(t *testing.T) {
	e := newJeEnv(t)
	ctx := testCtx(t)
	u1, u2, u3 := e.newUser(), e.newUser(), e.newUser()

	j1 := e.confirmN(u1, "m1", 2)
	r1, err := e.jobs.Start(ctx, j1.ID, "mod-job-1")
	if err != nil || r1.Status != jobs.StatusRunning {
		t.Fatalf("Start: %+v, %v", r1, err)
	}
	if ref := e.str("SELECT module_job_ref FROM platform.jobs WHERE id = $1", j1.ID); ref != "mod-job-1" {
		t.Fatalf("module_job_ref = %q", ref)
	}
	started := e.str("SELECT started_at::text FROM platform.jobs WHERE id = $1", j1.ID)
	// a replay, even with another reference: the same job, nothing written
	before := e.fingerprint()
	again, err := e.jobs.Start(ctx, j1.ID, "mod-job-OTHER")
	if err != nil || again.Status != jobs.StatusRunning || e.fingerprint() != before {
		t.Fatalf("a replay of Start: %+v, %v (fingerprint changed: %v)", again.Status, err, e.fingerprint() != before)
	}
	if ref := e.str("SELECT module_job_ref FROM platform.jobs WHERE id = $1", j1.ID); ref != "mod-job-1" || e.str("SELECT started_at::text FROM platform.jobs WHERE id = $1", j1.ID) != started {
		t.Fatalf("a replay changed the reference or the start time")
	}
	if n := e.transitionCount(j1.ID, jobs.StatusRunning); n != 1 {
		t.Fatalf("%d moves to running, want 1", n)
	}

	// the reference is unique per module: another module 1 job cannot hold it, and the refusal rolls the move back
	j2 := e.confirmN(u2, "m1", 2)
	_, err = e.jobs.Start(ctx, j2.ID, "mod-job-1")
	jeIs(t, "Start with a reference another job of the module holds", err, jobs.ErrModuleRefTaken)
	jeNoRawSQL(t, "the reference refusal", err)
	if e.jobStatus(j2.ID) != jobs.StatusConfirmed || e.str("SELECT COALESCE(module_job_ref, '-') FROM platform.jobs WHERE id = $1", j2.ID) != "-" {
		t.Fatal("a refused Start left something behind")
	}
	// the controls: another reference starts it; the same reference on ANOTHER module is no clash
	if r, err := e.jobs.Start(ctx, j2.ID, "mod-job-2"); err != nil || r.Status != jobs.StatusRunning {
		t.Fatalf("Start with a free reference: %v", err)
	}
	j3 := e.confirmN(u3, "m2", 1)
	if r, err := e.jobs.Start(ctx, j3.ID, "mod-job-1"); err != nil || r.Status != jobs.StatusRunning {
		t.Fatalf("the same reference on another module: %v", err)
	}

	// a draft, a terminal job and an unknown job
	u4, u5 := e.newUser(), e.newUser()
	d, err := e.jobs.CreateDraft(ctx, u4, jobs.CreateReq{Module: "m1", Rows: jeAddrs(1, "d")})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.jobs.Start(ctx, d.ID, "")
	jeNotAllowed(t, "Start of a draft", err, "job_transition_illegal")
	c := e.confirmN(u5, "m2", 1)
	if _, err := e.jobs.CancelBeforeStart(ctx, u5, c.ID); err != nil {
		t.Fatal(err)
	}
	_, err = e.jobs.Start(ctx, c.ID, "")
	jeNotAllowed(t, "Start of a cancelled job", err, "jobs_terminal_is_final", "job_transition_illegal")
	_, err = e.jobs.Start(ctx, "00000000-0000-4000-8000-0000000000ff", "")
	jeIs(t, "Start of an unknown job", err, jobs.ErrNotFound)
	_, err = e.jobs.Start(ctx, j1.ID, strings.Repeat("x", 201))
	jeIs(t, "Start with a hostile reference", err, jobs.ErrBadRequest)
}

func TestJobsEngine_RecordResultsSettlesTheLedgerAndIsIdempotent(t *testing.T) {
	e := newJeEnv(t)
	u := e.newUser()
	j := e.running(u, "m2", 4)
	ids := e.rowIDs(j.ID, "pending")
	if len(ids) != 4 {
		t.Fatalf("%d pending rows", len(ids))
	}
	if r, _, _ := e.reserveOf(j.ID); r != 4 {
		t.Fatalf("the reserve is %d, want 4 (4 rows at 1 credit)", r)
	}

	s := e.record(j.ID, jeItem(ids[0], jeM2Sent), jeItem(ids[1], jeM2Bounced), jeItem(ids[2], jeM2Failed), jeItem(ids[3], jeM2Queued))
	if want := (jobs.RecordSummary{Recorded: 3, Charged: 2, Credits: 2, NotFinal: 1}); s != want {
		t.Fatalf("summary = %+v, want %+v", s, want)
	}
	if e.chargeRows(j.ID) != 2 || e.resultCount(j.ID) != 3 {
		t.Fatalf("ledger charges %d, results %d; want 2 and 3", e.chargeRows(j.ID), e.resultCount(j.ID))
	}
	jeSameMap(t, "the ledger and the results that name a unit", e.ledgerCharges(j.ID), e.resultUnits(j.ID))
	jeSameMap(t, "the charges and what the module's words earn", e.ledgerCharges(j.ID), map[string]string{ids[0]: "m2_accepted", ids[1]: "m2_bounced"})
	if _, charged, released := e.reserveOf(j.ID); charged != 2 || released != 0 {
		t.Fatalf("reserve charged %d released %d", charged, released)
	}
	if n := e.int("SELECT count(*) FROM platform.job_results WHERE job_id = $1 AND item_ref = $2", j.ID, ids[3]); n != 0 {
		t.Fatal("an item that is not final was recorded")
	}
	// the row of each result: position and address from the job's row, the module's own words, the detail
	for i, want := range map[int]string{
		0: "module|sent|charge|m2_accepted|M2-01|v|1|r00001@example.test|-",
		1: "module|bounced|charge|m2_bounced|M2-02|v|2|r00002@example.test|-",
		2: "module|failed|free|-|M2-03|v|3|r00003@example.test|attempts_exhausted",
	} {
		got := e.str(`SELECT origin || '|' || module_state || '|' || decision || '|' || COALESCE(unit, '-') || '|' || rule || '|' || (detail->>'k') || '|' || ordinal::text || '|' || email || '|' || COALESCE(reason, '-')
			FROM platform.job_results WHERE job_id = $1 AND item_ref = $2`, j.ID, ids[i])
		if got != want {
			t.Errorf("result of row %d = %q, want %q", i+1, got, want)
		}
	}
	e.requireBooks("after the first page")

	// the same page again: nothing is written, the summary says so (the paired control of the idempotence: the first call wrote)
	before := e.fingerprint()
	s2 := e.record(j.ID, jeItem(ids[0], jeM2Sent), jeItem(ids[1], jeM2Bounced), jeItem(ids[2], jeM2Failed), jeItem(ids[3], jeM2Queued))
	if want := (jobs.RecordSummary{AlreadyRecorded: 3, NotFinal: 1}); s2 != want {
		t.Fatalf("the replay's summary = %+v, want %+v", s2, want)
	}
	if e.fingerprint() != before {
		t.Fatal("a replay of RecordResults wrote something")
	}
	// a second page: the item that was not final is final now, and one old item is repeated
	s3 := e.record(j.ID, jeItem(ids[3], jeM2Sent), jeItem(ids[0], jeM2Sent))
	if want := (jobs.RecordSummary{Recorded: 1, AlreadyRecorded: 1, Charged: 1, Credits: 1}); s3 != want {
		t.Fatalf("the second page's summary = %+v, want %+v", s3, want)
	}
	if e.chargeRows(j.ID) != 3 || e.resultCount(j.ID) != 4 {
		t.Fatalf("after the second page: %d charges, %d results; want 3 and 4", e.chargeRows(j.ID), e.resultCount(j.ID))
	}
	jeSameMap(t, "the ledger and the results after the second page", e.ledgerCharges(j.ID), e.resultUnits(j.ID))
	e.requireBooks("after the second page")
}

// The control of the drift view: it must be able to say "no". A charge written without its result is exactly what a settlement that is not one transaction with its results would leave behind; and
// the engine's path writes the result under the LEDGER's unit, whatever the module says now (the first charge stands).
func TestJobsEngine_SettlementDriftSeesAChargeWithoutItsResultAndTheEngineWritesTheResultUnderTheLedgersUnit(t *testing.T) {
	e := newJeEnv(t)
	u := e.newUser()
	j := e.running(u, "m2", 2)
	ids := e.rowIDs(j.ID, "pending")

	e.settle(j.ID, mnyItem(ids[0], mnyM2Sent), mnyItem(ids[1], mnyM2Bounced)) // the money store alone: charges, no results
	drift := e.rows(`SELECT problem FROM platform.job_settlement_drift ORDER BY 1`)
	if len(drift) != 2 || drift[0] != "charge_without_result" || drift[1] != "charge_without_result" {
		t.Fatalf("the drift view says %v; it must name the two charges that have no result", drift)
	}
	s := e.record(j.ID, jeItem(ids[0], jeM2Sent), jeItem(ids[1], jeM2Sent)) // row 2 was charged as bounced; the module now says sent: the first charge stands
	if s.Recorded != 2 || s.Charged != 0 {
		t.Fatalf("summary %+v: both results are new and nothing is charged twice", s)
	}
	jeSameMap(t, "the results and the ledger", e.resultUnits(j.ID), map[string]string{ids[0]: "m2_accepted", ids[1]: "m2_bounced"})
	e.requireBooks("after the engine recorded what the ledger already held")
}

func TestJobsEngine_AnUnreachableUnitIsRecordedUnmappedNeverChargedAndFlagged(t *testing.T) {
	e := newJeEnv(t) // the vendor unit is OFF
	u := e.newUser()
	j := e.running(u, "m1", 3)
	if prices := e.frozen(j.ID); len(prices) != 2 || prices["m1_vendor"] != 0 {
		t.Fatalf("the price snapshot is %v: the vendor unit must not be part of it", prices)
	}
	ids := e.rowIDs(j.ID, "pending")
	own := charge.Outcome{Module: "m1", State: "valid", Reason: "accepted"}             // M1-03: m1_own
	vendor := charge.Outcome{Module: "m1", State: "valid", Reason: "external_verified"} // M1-02: m1_vendor, which this job cannot be charged
	noOrigin := charge.Outcome{Module: "m1", State: "unknown", Reason: "greylisted"}    // M1-23: an unknown that names no origin
	s := e.record(j.ID, jeItem(ids[0], own), jeItem(ids[1], vendor), jeItem(ids[2], noOrigin))
	if want := (jobs.RecordSummary{Recorded: 3, Charged: 1, Credits: 1, Unmapped: 2}); s != want {
		t.Fatalf("summary = %+v, want %+v", s, want)
	}
	for i, want := range map[int]string{0: "charge|m1_own|M1-03", 1: "unmapped|-|M1-02", 2: "unmapped|-|M1-23"} {
		got := e.str("SELECT decision || '|' || COALESCE(unit, '-') || '|' || rule FROM platform.job_results WHERE job_id = $1 AND item_ref = $2", j.ID, ids[i])
		if got != want {
			t.Errorf("result of row %d = %q, want %q", i+1, got, want)
		}
	}
	if e.chargeRows(j.ID) != 1 || e.int("SELECT count(*) FROM platform.ledger WHERE job_id = $1 AND item_ref IN ($2, $3)", j.ID, ids[1], ids[2]) != 0 {
		t.Fatal("an unmapped item was charged")
	}
	flags := e.rows(`SELECT (detail->>'rule') || '|' || (detail->>'items') || '|' || actor_kind || '|' || (reason LIKE '%(rule ' || (detail->>'rule') || ')%')::text
		FROM platform.job_events WHERE job_id = $1 AND kind = 'flag' ORDER BY detail->>'rule'`, j.ID)
	if len(flags) != 2 || flags[0] != "M1-02|1|engine|true" || flags[1] != "M1-23|1|engine|true" {
		t.Fatalf("flag events = %v, want one per rule naming it, written by the engine", flags)
	}
	e.requireBooks("after the unmapped results")
	// a replay records nothing new, so it flags nothing new
	before := e.fingerprint()
	s2 := e.record(j.ID, jeItem(ids[0], own), jeItem(ids[1], vendor), jeItem(ids[2], noOrigin))
	if s2.Recorded != 0 || s2.AlreadyRecorded != 3 || s2.Unmapped != 0 || e.eventCount(j.ID, "flag") != 2 || e.fingerprint() != before {
		t.Fatalf("the replay: %+v, %d flags, fingerprint changed %v", s2, e.eventCount(j.ID, "flag"), e.fingerprint() != before)
	}

	// the control: with the vendor unit ON when the job is confirmed, the same answer is a charge of the vendor unit
	e.vendor(true)
	u2 := e.newUser()
	j2 := e.running(u2, "m1", 1)
	ids2 := e.rowIDs(j2.ID, "pending")
	s3 := e.record(j2.ID, jeItem(ids2[0], vendor))
	if s3.Recorded != 1 || s3.Charged != 1 || s3.Credits != 2 || s3.Unmapped != 0 {
		t.Fatalf("with the vendor reachable: %+v, want one charge of 2 credits", s3)
	}
	if got := e.str("SELECT decision || '|' || unit FROM platform.job_results WHERE job_id = $1", j2.ID); got != "charge|m1_vendor" || e.eventCount(j2.ID, "flag") != 0 {
		t.Fatalf("with the vendor reachable the result is %q and %d flags were written", got, e.eventCount(j2.ID, "flag"))
	}
	e.requireBooks("after the vendor control")
}

func TestJobsEngine_ItemsThatAreNotFinalOrNotRowsOfTheJobAreNeverRecordedOrCharged(t *testing.T) {
	e := newJeEnv(t)
	ctx := testCtx(t)
	u := e.newUser()
	rows := []jobs.RowIn{{Email: "a1@example.test"}, {Email: "not an address"}, {Email: "A1@example.test"}, {Email: "b2@example.test"}}
	j := e.confirmRows(u, "m2", rows)
	if _, err := e.jobs.Start(ctx, j.ID, ""); err != nil {
		t.Fatal(err)
	}
	all, pending := e.rowIDs(j.ID, ""), e.rowIDs(j.ID, "pending")
	if len(all) != 4 || len(pending) != 2 || e.resultCount(j.ID) != 2 {
		t.Fatalf("%d rows, %d pending, %d results at the start; want 4, 2 and the platform's 2", len(all), len(pending), e.resultCount(j.ID))
	}
	foreign := "00000000-0000-4000-8000-0000000000aa"
	s := e.record(j.ID,
		jeItem(pending[0], jeM2Queued),                   // not final
		jeItem(all[1], jeM2Sent),                         // the malformed row: not a pending row
		jeItem(all[2], jeM2Sent),                         // the duplicate row
		jeItem(foreign, jeM2Sent),                        // no row of this job
		jeItem("not-a-uuid", jeM2Sent),                   // no row can have this reference
		jeItem(pending[0], charge.Outcome{Module: "m2"}), // no state at all
		jeItem(pending[1], jeM2Sent))                     // the one good item
	if want := (jobs.RecordSummary{Recorded: 1, Charged: 1, Credits: 1, NotFinal: 1, Rejected: 5}); s != want {
		t.Fatalf("summary = %+v, want %+v", s, want)
	}
	if e.resultCount(j.ID) != 3 || e.chargeRows(j.ID) != 1 {
		t.Fatalf("%d results, %d charges; want 3 (the platform's two and the one good item) and 1", e.resultCount(j.ID), e.chargeRows(j.ID))
	}
	if n := e.int("SELECT count(*) FROM platform.job_results WHERE job_id = $1 AND origin = 'module' AND item_ref IN ($2, $3)", j.ID, all[1], all[2]); n != 0 {
		t.Fatal("the module wrote the result of a row the platform ended itself")
	}
	// not recorded is not lost: the item that was not final is recorded when it is
	if s := e.record(j.ID, jeItem(pending[0], jeM2Sent)); s.Recorded != 1 || s.Charged != 1 {
		t.Fatalf("the item that became final: %+v", s)
	}
	// the control of the module check: an outcome of another module refuses the whole call and writes nothing
	before := e.fingerprint()
	_, err := e.jobs.RecordResults(ctx, j.ID, []jobs.ResultItem{jeItem(pending[0], charge.Outcome{Module: "m1", State: "valid", Reason: "accepted"})})
	jeIs(t, "an outcome of another module", err, jobs.ErrBadRequest)
	if e.fingerprint() != before {
		t.Fatal("a refused call wrote something")
	}
	e.requireBooks("after the foreign items")
}

func TestJobsEngine_Module3IsRecordedFreeWithNoMoneyCall(t *testing.T) {
	e := newJeEnv(t)
	ctx := testCtx(t)
	u := e.newUser()
	j := e.running(u, "m3", 2)
	ids := e.rowIDs(j.ID, "pending")
	if e.int("SELECT count(*) FROM platform.reserves WHERE job_id = $1", j.ID) != 0 {
		t.Fatal("a module 3 job holds a reserve")
	}
	s := e.record(j.ID, jeItem(ids[0], charge.Outcome{Module: "m3", State: "sent"}), jeItem(ids[1], charge.Outcome{Module: "m3", State: "cancelled"}))
	if want := (jobs.RecordSummary{Recorded: 2}); s != want {
		t.Fatalf("summary = %+v, want %+v", s, want)
	}
	for i, want := range map[int]string{0: "free|-|M3-99", 1: "free|-|M3-01"} {
		if got := e.str("SELECT decision || '|' || COALESCE(unit, '-') || '|' || rule FROM platform.job_results WHERE job_id = $1 AND item_ref = $2", j.ID, ids[i]); got != want {
			t.Errorf("result of row %d = %q, want %q", i+1, got, want)
		}
	}
	if e.int("SELECT count(*) FROM platform.ledger WHERE job_id = $1", j.ID) != 0 {
		t.Fatal("a module 3 job wrote to the ledger")
	}
	if _, err := e.jobs.Complete(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	fin, err := e.jobs.Finish(ctx, j.ID)
	if err != nil || fin.Status != jobs.StatusSucceeded || fin.FinishedAt == nil {
		t.Fatalf("Finish of a module 3 job: %+v, %v", fin.Status, err)
	}
	if e.int("SELECT count(*) FROM platform.ledger WHERE job_id = $1", j.ID) != 0 {
		t.Fatal("finishing a module 3 job wrote to the ledger")
	}
	e.requireBooks("after a module 3 job")
	// the control: an outcome of a metered module in a module 3 job is refused
	u2 := e.newUser()
	j2 := e.running(u2, "m3", 1)
	_, err = e.jobs.RecordResults(ctx, j2.ID, []jobs.ResultItem{jeItem(e.rowIDs(j2.ID, "pending")[0], jeM2Sent)})
	jeIs(t, "a module 2 outcome in a module 3 job", err, jobs.ErrBadRequest)
}

func TestJobsEngine_RecordResultsIsForAJobThatIsRunningCancellingOrCompleting(t *testing.T) {
	e := newJeEnv(t)
	ctx := testCtx(t)
	u, u2 := e.newUser(), e.newUser()
	c := e.confirmN(u, "m2", 2) // confirmed, not started
	ids := e.rowIDs(c.ID, "pending")
	_, err := e.jobs.RecordResults(ctx, c.ID, []jobs.ResultItem{jeItem(ids[0], jeM2Sent)})
	jeNotAllowed(t, "RecordResults for a job that has not started", err, "not_settling")
	if _, err := e.jobs.CancelBeforeStart(ctx, u, c.ID); err != nil {
		t.Fatal(err)
	}
	_, err = e.jobs.RecordResults(ctx, c.ID, []jobs.ResultItem{jeItem(ids[0], jeM2Sent)})
	jeNotAllowed(t, "RecordResults for a cancelled job", err, "not_settling")
	if e.chargeRows(c.ID) != 0 {
		t.Fatal("a charge was written for a job that never ran")
	}
	// the control: the same call for a running job
	r := e.running(u2, "m2", 2)
	if s := e.record(r.ID, jeItem(e.rowIDs(r.ID, "pending")[0], jeM2Sent)); s.Recorded != 1 {
		t.Fatalf("a running job: %+v", s)
	}
}

func TestJobsEngine_CompleteRefusesWhileAPendingRowLacksAResult(t *testing.T) {
	e := newJeEnv(t)
	ctx := testCtx(t)
	u, u2 := e.newUser(), e.newUser()
	j := e.running(u, "m2", 3)
	ids := e.rowIDs(j.ID, "pending")
	e.record(j.ID, jeItem(ids[0], jeM2Sent), jeItem(ids[1], jeM2Sent))
	before := e.fingerprint()
	_, err := e.jobs.Complete(ctx, j.ID)
	var inc *jobs.IncompleteError
	if !errors.Is(err, jobs.ErrIncomplete) || !errors.As(err, &inc) || inc.Missing != 1 {
		t.Fatalf("Complete with one row missing: %v, want ErrIncomplete naming 1 row", err)
	}
	jeNoRawSQL(t, "Complete", err)
	if e.jobStatus(j.ID) != jobs.StatusRunning || e.fingerprint() != before {
		t.Fatal("a refused Complete changed something")
	}
	// the control: the last result arrives and the same call goes through
	e.record(j.ID, jeItem(ids[2], jeM2Failed))
	c, err := e.jobs.Complete(ctx, j.ID)
	if err != nil || c.Status != jobs.StatusCompleting {
		t.Fatalf("Complete with every result in: %v, %v", c.Status, err)
	}
	// idempotent when already completing
	before = e.fingerprint()
	c2, err := e.jobs.Complete(ctx, j.ID)
	if err != nil || c2.Status != jobs.StatusCompleting || e.fingerprint() != before || e.transitionCount(j.ID, jobs.StatusCompleting) != 1 {
		t.Fatalf("a replay of Complete: %v, %v, %d moves", c2.Status, err, e.transitionCount(j.ID, jobs.StatusCompleting))
	}
	// a job that is only confirmed is refused by the lifecycle, not by the result check
	k := e.confirmN(u2, "m2", 1)
	_, err = e.jobs.Complete(ctx, k.ID)
	jeNotAllowed(t, "Complete of a confirmed job", err, "job_transition_illegal")
}

func TestJobsEngine_FinishReleasesExactlyTheRemainderOnce(t *testing.T) {
	e := newJeEnv(t)
	ctx := testCtx(t)
	u, u2 := e.newUser(), e.newUser()
	j := e.running(u, "m2", 4)
	ids := e.rowIDs(j.ID, "pending")
	e.record(j.ID, jeItem(ids[0], jeM2Sent), jeItem(ids[1], jeM2Sent), jeItem(ids[2], jeM2Failed), jeItem(ids[3], jeM2Failed))
	e.requireBal("before the end", u, "m2", 996, 2)
	if _, err := e.jobs.Complete(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	fin, err := e.jobs.Finish(ctx, j.ID)
	if err != nil || fin.Status != jobs.StatusSucceeded || fin.FinishedAt == nil {
		t.Fatalf("Finish: %v, %v", fin.Status, err)
	}
	e.requireEnded(j.ID, jobs.StatusSucceeded, 1)
	if got := e.int("SELECT available_delta FROM platform.ledger WHERE job_id = $1 AND kind = 'release'", j.ID); got != 2 {
		t.Fatalf("the release gave back %d credits, want exactly the remainder 2", got)
	}
	e.requireBal("after the end", u, "m2", 998, 0)
	// a replay changes nothing and releases nothing more
	before := e.fingerprint()
	again, err := e.jobs.Finish(ctx, j.ID)
	if err != nil || again.Status != jobs.StatusSucceeded || e.fingerprint() != before || e.releaseRows(j.ID) != 1 {
		t.Fatalf("a replay of Finish: %v, %v, %d release rows, fingerprint changed %v", again.Status, err, e.releaseRows(j.ID), e.fingerprint() != before)
	}

	// Finish re-checks the results: a completing job (walked there by hand, as a crash or an administrator could leave it) with rows unanswered is refused and nothing changes
	k := e.confirmN(u2, "m2", 2)
	jobWalkTo(t, e.admin, k.ID, jobs.StatusCompleting)
	before = e.fingerprint()
	_, err = e.jobs.Finish(ctx, k.ID)
	var inc *jobs.IncompleteError
	if !errors.As(err, &inc) || inc.Missing != 2 {
		t.Fatalf("Finish with two rows unanswered: %v, want ErrIncomplete naming 2", err)
	}
	if e.jobStatus(k.ID) != jobs.StatusCompleting || e.releaseRows(k.ID) != 0 || e.fingerprint() != before {
		t.Fatal("a refused Finish changed something")
	}
	// the control: the results arrive (a completing job still records them) and Finish ends the job and gives the whole reserve back
	kids := e.rowIDs(k.ID, "pending")
	e.record(k.ID, jeItem(kids[0], jeM2Failed), jeItem(kids[1], jeM2Failed))
	if fin, err := e.jobs.Finish(ctx, k.ID); err != nil || fin.Status != jobs.StatusSucceeded {
		t.Fatalf("Finish with every result in: %v, %v", fin.Status, err)
	}
	e.requireEnded(k.ID, jobs.StatusSucceeded, 1)
	e.requireBal("after the second job", u2, "m2", 1000, 0)
	// a job that is neither completing nor finished is refused with a reason of the store's
	d := e.running(e.newUser(), "m2", 1)
	_, err = e.jobs.Finish(ctx, d.ID)
	jeNotAllowed(t, "Finish of a running job", err, "not_completing")
}

// The release and the move are ONE transaction: a release that fails after the move leaves the job completing and the reserve intact (what a process killed at the fault point release.mid_tx
// leaves). The fault is a trigger the test puts on the ledger as the superuser: it refuses a release. The control is the same call without it.
func TestJobsEngine_AFailedReleaseRollsTheMoveBackAndTheJobStaysCompleting(t *testing.T) {
	e := newJeEnv(t)
	ctx := testCtx(t)
	u := e.newUser()
	j := e.running(u, "m2", 3)
	ids := e.rowIDs(j.ID, "pending")
	e.record(j.ID, jeItem(ids[0], jeM2Sent), jeItem(ids[1], jeM2Sent), jeItem(ids[2], jeM2Failed))
	if _, err := e.jobs.Complete(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	if rem := e.remainder(j.ID); rem != 1 {
		t.Fatalf("the remainder is %d, want 1", rem)
	}
	e.exec(`CREATE FUNCTION platform.je_refuse_release() RETURNS trigger LANGUAGE plpgsql AS $fn$
		BEGIN IF NEW.kind = 'release' THEN RAISE EXCEPTION 'test: the release is refused' USING ERRCODE = 'P0001'; END IF; RETURN NEW; END $fn$`)
	e.exec(`CREATE TRIGGER je_refuse_release BEFORE INSERT ON platform.ledger FOR EACH ROW EXECUTE FUNCTION platform.je_refuse_release()`)

	before := e.fingerprint()
	_, err := e.jobs.Finish(ctx, j.ID)
	if err == nil || errors.Is(err, jobs.ErrIncomplete) {
		t.Fatalf("Finish with a release that fails: %v, want the release's error", err)
	}
	if got := e.jobStatus(j.ID); got != jobs.StatusCompleting {
		t.Fatalf("the job is %s after a release that failed: the move was not rolled back with it", got)
	}
	if e.fingerprint() != before || e.remainder(j.ID) != 1 || e.releaseRows(j.ID) != 0 || e.transitionCount(j.ID, jobs.StatusSucceeded) != 0 {
		t.Fatal("a failed Finish left something behind: the transition and the release are not one transaction")
	}
	if !ledgerBool(t, e.admin, "SELECT finished_at IS NULL FROM platform.jobs WHERE id = $1", j.ID) {
		t.Fatal("finished_at was written by a Finish that rolled back")
	}

	// the control: without the fault the same call finishes the job and releases the remainder
	e.exec("DROP TRIGGER je_refuse_release ON platform.ledger")
	e.exec("DROP FUNCTION platform.je_refuse_release()")
	fin, err := e.jobs.Finish(ctx, j.ID)
	if err != nil || fin.Status != jobs.StatusSucceeded {
		t.Fatalf("Finish after the fault was removed: %v, %v", fin.Status, err)
	}
	e.requireEnded(j.ID, jobs.StatusSucceeded, 1)
	e.requireBal("after the repeat", u, "m2", 998, 0)
}

func TestJobsEngine_ExpireWritesP02ResultsForTheRowsNoOneAnsweredAndReleasesEverything(t *testing.T) {
	e := newJeEnv(t, jobs.WithMaxStartDelay(3*time.Second))
	ctx := testCtx(t)
	u := e.newUser()
	rows := []jobs.RowIn{{Email: "a@example.test"}, {Email: "not an address"}, {Email: "A@example.test"}, {Email: "b@example.test"}}
	j := e.confirmRows(u, "m2", rows) // 2 pending rows (a reserve of 2), a malformed row and a duplicate whose results the confirm wrote
	if j.Status != jobs.StatusConfirmed || e.resultCount(j.ID) != 2 {
		t.Fatalf("the fixture: %s with %d results", j.Status, e.resultCount(j.ID))
	}

	// before the deadline the database refuses, and nothing changes
	before := e.fingerprint()
	_, err := e.jobs.Expire(ctx, j.ID)
	jeNotAllowed(t, "Expire before the start deadline", err, "job_expired_early")
	if e.fingerprint() != before {
		t.Fatal("a refused Expire wrote something")
	}
	if w, ok := e.workOf(j.ID); !ok || w.DeadlinePassed {
		t.Fatalf("before its deadline the job is work (%v) with DeadlinePassed false (%v)", ok, w.DeadlinePassed)
	}

	// the control: the deadline passes (the database's clock)
	time.Sleep(3500 * time.Millisecond)
	if w, ok := e.workOf(j.ID); !ok || !w.DeadlinePassed {
		t.Fatalf("after its deadline the job must be work with DeadlinePassed true (listed %v, %v)", ok, w.DeadlinePassed)
	}
	ex, err := e.jobs.Expire(ctx, j.ID)
	if err != nil || ex.Status != jobs.StatusExpired || ex.FinishedAt == nil {
		t.Fatalf("Expire after the deadline: %v, %v", ex.Status, err)
	}
	got := e.rows(`SELECT ordinal::text || '|' || origin || '|' || module_state || '|' || decision || '|' || rule FROM platform.job_results WHERE job_id = $1 ORDER BY ordinal`, j.ID)
	want := []string{"1|platform|expired|free|P-02", "2|platform|malformed|free|P-08", "3|platform|duplicate|free|P-07", "4|platform|expired|free|P-02"}
	if strings.Join(got, ";") != strings.Join(want, ";") {
		t.Fatalf("results = %v, want %v (the pending rows P-02, the others keep theirs)", got, want)
	}
	e.requireEnded(j.ID, jobs.StatusExpired, 1)
	e.requireBal("after the expiry", u, "m2", 1000, 0)
	before = e.fingerprint()
	if again, err := e.jobs.Expire(ctx, j.ID); err != nil || again.Status != jobs.StatusExpired || e.fingerprint() != before {
		t.Fatalf("a replay of Expire: %v, %v", again.Status, err)
	}

	// scheduled and paused jobs (hand-built with a deadline that has passed) expire the same way; a paused one inside its deadline and a running one do not
	us, up, uq, ur := e.newUser(), e.newUser(), e.newUser(), e.newUser()
	sid, pid, qid := "0e000000-0000-4000-8000-000000000001", "0e000000-0000-4000-8000-000000000002", "0e000000-0000-4000-8000-000000000003"
	jobInsertAtWith(t, e.admin, sid, us, "m2", "scheduled", jobOpts{Deadline: jobDeadlinePast})
	jobInsertAtWith(t, e.admin, pid, up, "m2", "paused", jobOpts{Deadline: jobDeadlinePast})
	jobInsertAt(t, e.admin, qid, uq, "m2", "paused")
	for _, id := range []string{sid, pid} {
		if r, err := e.jobs.Expire(ctx, id); err != nil || r.Status != jobs.StatusExpired {
			t.Errorf("Expire of %s: %v, %v", id, r.Status, err)
		}
	}
	_, err = e.jobs.Expire(ctx, qid)
	jeNotAllowed(t, "Expire of a paused job inside its deadline", err, "job_expired_early")
	run := e.running(ur, "m2", 1)
	_, err = e.jobs.Expire(ctx, run.ID)
	jeNotAllowed(t, "Expire of a running job", err, "job_transition_illegal")
}

func TestJobsEngine_FailJobEndsTheJobFreeForWhatWasNotSettledAndKeepsWhatWasCharged(t *testing.T) {
	e := newJeEnv(t)
	ctx := testCtx(t)
	u, u2, u3 := e.newUser(), e.newUser(), e.newUser()
	j := e.running(u, "m2", 4)
	ids := e.rowIDs(j.ID, "pending")
	e.record(j.ID, jeItem(ids[0], jeM2Sent), jeItem(ids[1], jeM2Sent))

	_, err := e.jobs.FailJob(ctx, j.ID, "  \n ")
	jeIs(t, "FailJob with a blank reason", err, jobs.ErrBadRequest)
	if e.jobStatus(j.ID) != jobs.StatusRunning {
		t.Fatal("a refused FailJob moved the job")
	}
	fj, err := e.jobs.FailJob(ctx, j.ID, "the module refused\nthe job: test")
	if err != nil || fj.Status != jobs.StatusFailed || fj.FinishedAt == nil {
		t.Fatalf("FailJob: %v, %v", fj.Status, err)
	}
	got := e.rows(`SELECT ordinal::text || '|' || origin || '|' || module_state || '|' || decision || '|' || COALESCE(unit, '-') || '|' || rule FROM platform.job_results WHERE job_id = $1 ORDER BY ordinal`, j.ID)
	want := []string{"1|module|sent|charge|m2_accepted|M2-01", "2|module|sent|charge|m2_accepted|M2-01", "3|platform|infrastructure_failure|free|-|P-04", "4|platform|infrastructure_failure|free|-|P-04"}
	if strings.Join(got, ";") != strings.Join(want, ";") {
		t.Fatalf("results = %v, want %v", got, want)
	}
	if r, c, l := e.reserveOf(j.ID); r != 4 || c != 2 || l != 2 {
		t.Fatalf("reserve held %d, charged %d, released %d; want 4, 2 (what was charged stays charged) and 2", r, c, l)
	}
	e.requireEnded(j.ID, jobs.StatusFailed, 1)
	e.requireBal("after the failure", u, "m2", 998, 0)
	if why := e.str("SELECT reason FROM platform.job_events WHERE job_id = $1 AND kind = 'transition' AND to_status = 'failed'", j.ID); why != "the module refused the job: test" {
		t.Errorf("the event's reason = %q", why)
	}
	before := e.fingerprint()
	if again, err := e.jobs.FailJob(ctx, j.ID, "again"); err != nil || again.Status != jobs.StatusFailed || e.fingerprint() != before {
		t.Fatalf("a replay of FailJob: %v, %v", again.Status, err)
	}

	// a job that never started: every row ends P-04 and the whole reserve goes back
	k := e.confirmN(u2, "m2", 3)
	if _, err := e.jobs.FailJob(ctx, k.ID, "the module refused the job"); err != nil {
		t.Fatal(err)
	}
	if n := e.int("SELECT count(*) FROM platform.job_results WHERE job_id = $1 AND rule = 'P-04' AND origin = 'platform'", k.ID); n != 3 {
		t.Fatalf("%d P-04 results, want 3", n)
	}
	e.requireEnded(k.ID, jobs.StatusFailed, 1)
	e.requireBal("after the failure of a job that never started", u2, "m2", 1000, 0)

	// a cancelled job cannot be failed, an unknown job is not found
	c := e.confirmN(u3, "m2", 1)
	if _, err := e.jobs.CancelBeforeStart(ctx, u3, c.ID); err != nil {
		t.Fatal(err)
	}
	_, err = e.jobs.FailJob(ctx, c.ID, "too late")
	jeNotAllowed(t, "FailJob of a cancelled job", err, "jobs_terminal_is_final", "job_transition_illegal")
	_, err = e.jobs.FailJob(ctx, "00000000-0000-4000-8000-0000000000ff", "x")
	jeIs(t, "FailJob of an unknown job", err, jobs.ErrNotFound)
}

func TestJobsEngine_TheRepairSetHoldsAnEndedJobWithARemainderAndNotAReleasedOne(t *testing.T) {
	e := newJeEnv(t)
	ctx := testCtx(t)
	u, u2, u3, u4 := e.newUser(), e.newUser(), e.newUser(), e.newUser()

	j := e.confirmN(u, "m2", 3)
	jobWalkTo(t, e.admin, j.ID, jobs.StatusFailed) // an administrator ended it by hand: its reserve was never released
	w, ok := e.workOf(j.ID)
	if !ok || w.Status != jobs.StatusFailed || w.ReserveRemainder != 3 {
		t.Fatalf("the job ended by hand: listed %v, status %s, remainder %d; want listed, failed, 3", ok, w.Status, w.ReserveRemainder)
	}
	// the control: a job the engine ended and released is not in the set
	k := e.confirmN(u2, "m2", 2)
	if _, err := e.jobs.FailJob(ctx, k.ID, "the module refused the job"); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.workOf(k.ID); ok {
		t.Fatal("a released job is in the repair set")
	}

	if err := e.jobs.Release(ctx, j.ID); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if e.remainder(j.ID) != 0 || e.releaseRows(j.ID) != 1 {
		t.Fatalf("after Release: remainder %d, %d release rows", e.remainder(j.ID), e.releaseRows(j.ID))
	}
	e.requireBal("after the repair", u, "m2", 1000, 0)
	if _, ok := e.workOf(j.ID); ok {
		t.Fatal("a repaired job is still in the repair set")
	}
	before := e.fingerprint()
	if err := e.jobs.Release(ctx, j.ID); err != nil || e.fingerprint() != before {
		t.Fatalf("a replay of Release: %v (fingerprint changed %v)", err, e.fingerprint() != before)
	}
	e.requireBooks("after the repair")

	// a running job is not Release's: the lifecycle is not asked, the store says so
	r := e.running(u3, "m2", 1)
	jeNotAllowed(t, "Release of a running job", e.jobs.Release(ctx, r.ID), "not_ended")
	// module 3 holds no credits: its ended job has nothing to release, and Release says nothing about it
	m := e.confirmN(u4, "m3", 1)
	if _, err := e.jobs.FailJob(ctx, m.ID, "the module refused the job"); err != nil {
		t.Fatal(err)
	}
	if err := e.jobs.Release(ctx, m.ID); err != nil {
		t.Fatalf("Release of an ended module 3 job: %v", err)
	}
	if err := e.jobs.Release(ctx, "00000000-0000-4000-8000-0000000000ff"); !errors.Is(err, jobs.ErrNotFound) {
		t.Fatalf("Release of an unknown job: %v", err)
	}
}

func TestJobsEngine_EngineWorkIsTheJobsTheEngineMustLookAtWithTheirCountsOldestProgressFirst(t *testing.T) {
	e := newJeEnv(t)
	ctx := testCtx(t)
	user := func() string { return e.newUser() }
	soon := time.Now().Add(time.Hour)

	a := e.confirmN(user(), "m2", 3) // confirmed
	b := e.running(user(), "m2", 4)  // running, with a stamp and a result
	bIDs := e.rowIDs(b.ID, "pending")
	if err := e.jobs.MarkSubmitted(ctx, b.ID, bIDs[:2]); err != nil {
		t.Fatal(err)
	}
	e.record(b.ID, jeItem(bIDs[0], jeM2Sent))
	cu := user()
	c := e.running(cu, "m2", 2)
	jobWalkTo(t, e.admin, c.ID, jobs.StatusCancelling)
	d := e.running(user(), "m2", 2)
	jobWalkTo(t, e.admin, d.ID, jobs.StatusCompleting)
	notDue := e.confirmRows(user(), "m2", jeAddrs(1, "n"), jeConfirmOpts{Start: &soon}) // scheduled, its time has not come
	due := e.confirmRows(user(), "m2", jeAddrs(1, "q"), jeConfirmOpts{Start: &soon})    // scheduled, its time has come
	jobMakeDue(t, e.admin, due.ID)
	pausedAhead := e.confirmRows(user(), "m2", jeAddrs(1, "p"), jeConfirmOpts{Start: &soon})
	jobWalkTo(t, e.admin, pausedAhead.ID, jobs.StatusPaused)
	pausedPast := "0e000000-0000-4000-8000-0000000000a1"
	jobInsertAtWith(t, e.admin, pausedPast, user(), "m2", "paused", jobOpts{Deadline: jobDeadlinePast})
	done := e.running(user(), "m2", 1) // succeeded and released
	e.record(done.ID, jeItem(e.rowIDs(done.ID, "pending")[0], jeM2Sent))
	if _, err := e.jobs.Complete(ctx, done.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.jobs.Finish(ctx, done.ID); err != nil {
		t.Fatal(err)
	}
	byHand := e.confirmN(user(), "m2", 2) // ended by hand with its reserve held
	jobWalkTo(t, e.admin, byHand.ID, jobs.StatusFailed)
	draftUser := user()
	draft, err := e.jobs.CreateDraft(ctx, draftUser, jobs.CreateReq{Module: "m2", Rows: jeAddrs(1, "x"), Content: jeContent()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.jobs.Preview(ctx, draftUser, draft.ID); err != nil { // previewed
		t.Fatal(err)
	}
	disc := user()
	dj, err := e.jobs.CreateDraft(ctx, disc, jobs.CreateReq{Module: "m2", Rows: jeAddrs(1, "y"), Content: jeContent()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.jobs.DiscardDraft(ctx, disc, dj.ID); err != nil { // cancelled before it was ever confirmed
		t.Fatal(err)
	}

	// oldest progress first: d, then b, then every other job (their progress is the moment of the test)
	e.exec("UPDATE platform.jobs SET last_progress_at = now() - interval '2 hours' WHERE id = $1", d.ID)
	e.exec("UPDATE platform.jobs SET last_progress_at = now() - interval '1 hour' WHERE id = $1", b.ID)

	work := e.work(1000)
	got := make([]string, len(work))
	for i, w := range work {
		got[i] = w.ID
	}
	wantSet := []string{a.ID, b.ID, c.ID, d.ID, due.ID, pausedPast, byHand.ID}
	sorted := append([]string(nil), got...)
	sort.Strings(sorted)
	sort.Strings(wantSet)
	if strings.Join(sorted, ",") != strings.Join(wantSet, ",") {
		t.Fatalf("the work is %v, want exactly the confirmed, running, cancelling and completing jobs, the scheduled job whose time has come, the paused job past its deadline and the ended job with a remainder (%v); not-due %s, paused-ahead %s, released %s, draft %s, discarded %s",
			got, wantSet, notDue.ID, pausedAhead.ID, done.ID, draft.ID, dj.ID)
	}
	if got[0] != d.ID || got[1] != b.ID {
		t.Errorf("the order is %v: want the completing job (2 hours without progress) first and the running one (1 hour) second", got[:2])
	}
	if two := e.work(2); len(two) != 2 || two[0].ID != d.ID || two[1].ID != b.ID {
		t.Errorf("a limit of 2 returned %d jobs", len(two))
	}
	if _, err := e.jobs.EngineWork(ctx, -1); !errors.Is(err, jobs.ErrBadRequest) {
		t.Errorf("a negative limit: %v", err)
	}

	// the counts, read in the same statement
	for _, w := range work {
		switch w.ID {
		case a.ID:
			if w.Rows != 3 || w.Pending != 3 || w.Submitted != 0 || w.Results != 0 || w.PendingResults != 0 || w.Unsubmitted != 3 || !w.Metered || w.ReserveRemainder != 3 || w.Content != nil || w.Module != "m2" || w.Unresolved() != 3 {
				t.Errorf("the confirmed job's counts: %+v", w)
			}
		case b.ID:
			if w.Rows != 4 || w.Pending != 4 || w.Submitted != 2 || w.Results != 1 || w.PendingResults != 1 || w.Unsubmitted != 2 || w.ReserveRemainder != 3 || w.Unresolved() != 3 || w.LastProgressAt == nil {
				t.Errorf("the running job's counts: %+v", w)
			}
		case pausedPast:
			if !w.DeadlinePassed || w.Status != jobs.StatusPaused {
				t.Errorf("the paused job: %s, deadline passed %v", w.Status, w.DeadlinePassed)
			}
		case byHand.ID:
			if w.Status != jobs.StatusFailed || w.ReserveRemainder != 2 {
				t.Errorf("the job ended by hand: %s, remainder %d", w.Status, w.ReserveRemainder)
			}
		}
	}
	one, err := e.jobs.EngineJobByID(ctx, b.ID)
	if err != nil || one.Rows != 4 || one.Submitted != 2 || one.PendingResults != 1 {
		t.Errorf("EngineJobByID: %+v, %v", one, err)
	}
	if _, err := e.jobs.EngineJobByID(ctx, "00000000-0000-4000-8000-0000000000ff"); !errors.Is(err, jobs.ErrNotFound) {
		t.Errorf("EngineJobByID of an unknown job: %v", err)
	}
}

func TestJobsEngine_EnsureModuleItemsWritesOneRowPerIdOnceWithTheCampaignsMessage(t *testing.T) {
	e := newJeEnv(t)
	ctx := testCtx(t)
	u := e.newUser()
	j := e.confirmRows(u, "m2", append(jeAddrs(3, "i"), jobs.RowIn{Email: "bad address"})) // 3 pending rows and a malformed one
	ids := e.rowIDs(j.ID, "pending")
	item := func(id string) string {
		return e.str(`SELECT user_id::text || '|' || campaign_id::text || '|' || recipient_email || '|' || rendered_subject || '|' || rendered_body || '|' || COALESCE(rendered_text, '<NULL>') || '|' || COALESCE(reply_to, '<NULL>') || '|' || status
			FROM public.job_items WHERE id = $1`, id)
	}
	n, err := e.jobs.EnsureModuleItems(ctx, j.ID, ids)
	if err != nil || n != 3 {
		t.Fatalf("EnsureModuleItems: %d, %v; want 3", n, err)
	}
	if got, want := item(ids[0]), u+"|"+j.ID+"|i00001@example.test|Hello from the engine tests|<p>hello</p>|<NULL>|reply@example.test|pending"; got != want {
		t.Fatalf("item = %q, want %q (a blank text is NULL)", got, want)
	}
	if e.int("SELECT count(*) FROM public.job_items WHERE campaign_id = $1", j.ID) != 3 {
		t.Fatal("not one public.job_items row per pending row")
	}
	// the same ids again, and a subset: one row per id still, nothing written
	before := e.fingerprint()
	if n, err := e.jobs.EnsureModuleItems(ctx, j.ID, ids); err != nil || n != 0 {
		t.Fatalf("the repeat: %d, %v", n, err)
	}
	if n, err := e.jobs.EnsureModuleItems(ctx, j.ID, ids[:1]); err != nil || n != 0 || e.fingerprint() != before {
		t.Fatalf("a subset: %d, %v", n, err)
	}
	// every refusal is typed and writes nothing: a row that is not pending, an id that is not canonical, a job that is not a send job, a job in a state that holds no rows, an unknown job
	all := e.rowIDs(j.ID, "")
	_, err = e.jobs.EnsureModuleItems(ctx, j.ID, all)
	jeIs(t, "a malformed row among the ids", err, jobs.ErrBadRequest)
	_, err = e.jobs.EnsureModuleItems(ctx, j.ID, []string{strings.ToUpper(ids[0])})
	jeIs(t, "an id that is not canonical", err, jobs.ErrBadRequest)
	_, err = e.jobs.EnsureModuleItems(ctx, j.ID, []string{"00000000-0000-4000-8000-0000000000ff"})
	jeIs(t, "an id that is no row of the job", err, jobs.ErrBadRequest)
	_, err = e.jobs.EnsureModuleItems(ctx, "00000000-0000-4000-8000-0000000000ff", ids[:1])
	jeIs(t, "an unknown job", err, jobs.ErrNotFound)
	m1 := e.confirmN(e.newUser(), "m1", 1)
	_, err = e.jobs.EnsureModuleItems(ctx, m1.ID, e.rowIDs(m1.ID, "pending"))
	jeNotAllowed(t, "a module 1 job", err, "not_a_send_job")
	if e.int("SELECT count(*) FROM public.job_items WHERE campaign_id = $1", j.ID) != 3 || e.int("SELECT count(*) FROM public.job_items WHERE campaign_id = $1", m1.ID) != 0 {
		t.Fatal("a refused call wrote rows")
	}

	// the text part: a real text is kept as it is, white space only is NULL, a text-only message has an empty body, no reply-to is NULL
	for name, c := range map[string]struct {
		content jobs.Content
		want    string
	}{
		"a real text":      {jobs.Content{Subject: "S1", HTML: "<p>x</p>", Text: "plain text part", ReplyTo: "r@example.test"}, "S1|<p>x</p>|plain text part|r@example.test"},
		"a blank text":     {jobs.Content{Subject: "S2", HTML: "<p>x</p>", Text: "  \n\t ", ReplyTo: ""}, "S2|<p>x</p>|<NULL>|<NULL>"},
		"a text-only body": {jobs.Content{Subject: "S3", HTML: "", Text: "only text"}, "S3||only text|<NULL>"},
	} {
		c := c
		k := e.confirmRows(e.newUser(), "m2", jeAddrs(1, "t"), jeConfirmOpts{Content: &c.content})
		kids := e.rowIDs(k.ID, "pending")
		if n, err := e.jobs.EnsureModuleItems(ctx, k.ID, kids); err != nil || n != 1 {
			t.Fatalf("%s: %d, %v", name, n, err)
		}
		got := e.str(`SELECT rendered_subject || '|' || rendered_body || '|' || COALESCE(rendered_text, '<NULL>') || '|' || COALESCE(reply_to, '<NULL>') FROM public.job_items WHERE id = $1`, kids[0])
		if got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}

	// a job in cancelling holds no more rows to hand over
	cu := e.newUser()
	r := e.running(cu, "m2", 2)
	if _, err := e.jobs.CancelBeforeStart(ctx, cu, r.ID); err != nil {
		t.Fatal(err)
	}
	_, err = e.jobs.EnsureModuleItems(ctx, r.ID, e.rowIDs(r.ID, "pending"))
	jeNotAllowed(t, "a cancelling job", err, "not_submittable")
}

type jeRenderer func(c jobs.Content, row jobs.EngineRow) (string, string, string, error)

func (f jeRenderer) Render(c jobs.Content, row jobs.EngineRow) (string, string, string, error) {
	return f(c, row)
}

// The renderer is the hook of Phase 5: what the store writes is what the renderer returned for the row, and a renderer that fails or returns what the shared table cannot hold refuses the call.
func TestJobsEngine_EnsureModuleItemsWritesWhatTheRendererReturnsAndRefusesWhatItCannot(t *testing.T) {
	e := newJeEnv(t)
	ctx := testCtx(t)
	perRow := jeRenderer(func(c jobs.Content, row jobs.EngineRow) (string, string, string, error) {
		return fmt.Sprintf("%s #%d", c.Subject, row.Ordinal), "<p>" + row.EmailNorm + "</p>", "text for " + row.EmailNorm, nil
	})
	st, err := jobs.New(e.pool, e.store, jobs.WithRenderer(perRow))
	if err != nil {
		t.Fatal(err)
	}
	k := e.confirmN(e.newUser(), "m2", 2)
	kids := e.rowIDs(k.ID, "pending")
	if n, err := st.EnsureModuleItems(ctx, k.ID, kids); err != nil || n != 2 {
		t.Fatalf("EnsureModuleItems: %d, %v", n, err)
	}
	if got := e.str("SELECT rendered_subject || '|' || rendered_body || '|' || rendered_text FROM public.job_items WHERE id = $1", kids[1]); got != "Hello from the engine tests #2|<p>r00002@example.test</p>|text for r00002@example.test" {
		t.Errorf("the renderer's output for row 2 = %q", got)
	}

	for name, fn := range map[string]jeRenderer{
		"a renderer that fails": func(jobs.Content, jobs.EngineRow) (string, string, string, error) {
			return "", "", "", errors.New("merge field of someone@example.test is missing")
		},
		"a blank subject":    func(jobs.Content, jobs.EngineRow) (string, string, string, error) { return "  ", "<p>x</p>", "", nil },
		"a subject on lines": func(jobs.Content, jobs.EngineRow) (string, string, string, error) { return "a\nb", "<p>x</p>", "", nil },
		"no body":            func(jobs.Content, jobs.EngineRow) (string, string, string, error) { return "S", "", " ", nil },
		"a NUL":              func(jobs.Content, jobs.EngineRow) (string, string, string, error) { return "S", "a\x00b", "", nil },
		"a body over a MiB": func(jobs.Content, jobs.EngineRow) (string, string, string, error) {
			return "S", strings.Repeat("a", jobs.MaxBodyBytes+1), "", nil
		},
	} {
		bad, err := jobs.New(e.pool, e.store, jobs.WithRenderer(fn))
		if err != nil {
			t.Fatal(err)
		}
		u := e.newUser()
		j := e.confirmN(u, "m2", 2)
		ids := e.rowIDs(j.ID, "pending")
		_, err = bad.EnsureModuleItems(ctx, j.ID, ids)
		var br *jobs.BadRequestError
		if !errors.As(err, &br) || !errors.Is(err, jobs.ErrBadRequest) || !strings.Contains(br.Reason, "row 1") {
			t.Errorf("%s: got %v, want ErrBadRequest naming row 1", name, err)
			continue
		}
		if strings.Contains(err.Error(), "someone@") || strings.Contains(err.Error(), "@") {
			t.Errorf("%s: the refusal quotes an address: %v", name, err)
		}
		if n := e.int("SELECT count(*) FROM public.job_items WHERE campaign_id = $1", j.ID); n != 0 {
			t.Errorf("%s: %d rows were written by a refused call", name, n)
		}
	}
}

func TestJobsEngine_PendingRowsAndMarkSubmittedHandTheRowsOverOnce(t *testing.T) {
	e := newJeEnv(t)
	ctx := testCtx(t)
	u := e.newUser()
	rows := jeAddrs(5, "f")
	rows[0].Fields = map[string]string{"first": "Ann"}
	j := e.confirmRows(u, "m2", rows)
	if _, err := e.jobs.Start(ctx, j.ID, ""); err != nil {
		t.Fatal(err)
	}
	ids := e.rowIDs(j.ID, "pending")

	got, err := e.jobs.PendingRows(ctx, j.ID, 2)
	if err != nil || len(got) != 2 || got[0].ID != ids[0] || got[0].Ordinal != 1 || got[0].EmailNorm != "f00001@example.test" || got[0].Fields["first"] != "Ann" || got[1].Ordinal != 2 || got[1].Fields == nil || len(got[1].Fields) != 0 {
		t.Fatalf("PendingRows(2) = %+v, %v; want rows 1 and 2 by ordinal with the merge data", got, err)
	}
	if err := e.jobs.MarkSubmitted(ctx, j.ID, ids[:2]); err != nil {
		t.Fatal(err)
	}
	got, err = e.jobs.PendingRows(ctx, j.ID, 10)
	if err != nil || len(got) != 3 || got[0].ID != ids[2] {
		t.Fatalf("after two were stamped: %+v, %v; want the other three", got, err)
	}
	// stamping twice is not an error and does not move the stamp
	stamp := e.str("SELECT submitted_at::text FROM platform.job_rows WHERE id = $1", ids[0])
	time.Sleep(30 * time.Millisecond)
	if err := e.jobs.MarkSubmitted(ctx, j.ID, ids[:2]); err != nil {
		t.Fatalf("the second stamp: %v", err)
	}
	if e.str("SELECT submitted_at::text FROM platform.job_rows WHERE id = $1", ids[0]) != stamp {
		t.Fatal("the second MarkSubmitted moved the stamp")
	}
	// a row of another job and an unknown row are left alone, not an error
	other := e.running(e.newUser(), "m2", 1)
	oid := e.rowIDs(other.ID, "pending")[0]
	if err := e.jobs.MarkSubmitted(ctx, j.ID, []string{ids[2], oid, "00000000-0000-4000-8000-0000000000bb"}); err != nil {
		t.Fatalf("MarkSubmitted with a foreign row: %v", err)
	}
	if !ledgerBool(t, e.admin, "SELECT submitted_at IS NOT NULL FROM platform.job_rows WHERE id = $1", ids[2]) || !ledgerBool(t, e.admin, "SELECT submitted_at IS NULL FROM platform.job_rows WHERE id = $1", oid) {
		t.Fatal("the stamp went to the wrong rows")
	}
	// a row that has a result is not offered again, even unstamped
	e.record(j.ID, jeItem(ids[3], jeM2Sent))
	got, err = e.jobs.PendingRows(ctx, j.ID, 10)
	if err != nil || len(got) != 1 || got[0].ID != ids[4] {
		t.Fatalf("PendingRows after a result: %+v, %v; want only the last row", got, err)
	}
	// the typed refusals
	if _, err := e.jobs.PendingRows(ctx, j.ID, 0); !errors.Is(err, jobs.ErrBadRequest) {
		t.Errorf("a limit of 0: %v", err)
	}
	if err := e.jobs.MarkSubmitted(ctx, j.ID, []string{"x"}); !errors.Is(err, jobs.ErrBadRequest) {
		t.Errorf("an id that is no uuid: %v", err)
	}
	if err := e.jobs.MarkSubmitted(ctx, "00000000-0000-4000-8000-0000000000ff", []string{ids[0]}); !errors.Is(err, jobs.ErrNotFound) {
		t.Errorf("an unknown job: %v", err)
	}
	// a draft's rows are nobody's to stamp
	d, err := e.jobs.CreateDraft(ctx, e.newUser(), jobs.CreateReq{Module: "m1", Rows: jeAddrs(1, "d")})
	if err != nil {
		t.Fatal(err)
	}
	jeNotAllowed(t, "MarkSubmitted of a draft's row", e.jobs.MarkSubmitted(ctx, d.ID, e.rowIDs(d.ID, "pending")), "not_submittable")
	// a cancelling job is offered no more rows
	if _, err := e.jobs.CancelBeforeStart(ctx, u, j.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := e.jobs.PendingRows(ctx, j.ID, 10); err != nil || len(got) != 0 {
		t.Fatalf("PendingRows of a cancelling job: %+v, %v; want none", got, err)
	}
}

// The module's own handle of a job (module 1's job id) is written by Start into platform.jobs.module_job_ref, and it is read BACK with the job: EngineJobByID and EngineWork both carry it, so that a
// module needs no memory and no database of its own to find the job it holds (the engine hands Observe the job as the store read it). A job started with none shows "" (a NULL column must not break
// the scan), and a job never shows another job's handle.
func TestJobsEngine_EngineJobCarriesTheModuleRef(t *testing.T) {
	e := newJeEnv(t)
	ctx := testCtx(t)
	u1, u2, u3 := e.newUser(), e.newUser(), e.newUser()
	byID := func(job string) jobs.EngineJob {
		t.Helper()
		j, err := e.jobs.EngineJobByID(ctx, job)
		if err != nil {
			t.Fatalf("EngineJobByID(%s): %v", job, err)
		}
		return j
	}
	inWork := func(job string) jobs.EngineJob {
		t.Helper()
		j, ok := e.workOf(job)
		if !ok {
			t.Fatalf("job %s is not in the engine's work", job)
		}
		return j
	}

	// a job that has not started carries none, in both reads
	j1 := e.confirmN(u1, "m1", 2)
	if got := byID(j1.ID).ModuleRef; got != "" {
		t.Fatalf("EngineJobByID of a job that has not started: ModuleRef %q, want none", got)
	}
	if got := inWork(j1.ID).ModuleRef; got != "" {
		t.Fatalf("EngineWork of a job that has not started: ModuleRef %q, want none", got)
	}
	// Start with a reference: the next reads of the job carry it, both of them, and it is what the table holds
	if _, err := e.jobs.Start(ctx, j1.ID, "mod-job-carried"); err != nil {
		t.Fatal(err)
	}
	if got := byID(j1.ID).ModuleRef; got != "mod-job-carried" {
		t.Errorf("EngineJobByID after Start: ModuleRef %q, want the reference Start recorded", got)
	}
	if got := inWork(j1.ID).ModuleRef; got != "mod-job-carried" {
		t.Errorf("EngineWork after Start: ModuleRef %q, want the reference Start recorded", got)
	}
	if col := e.str("SELECT module_job_ref FROM platform.jobs WHERE id = $1", j1.ID); col != byID(j1.ID).ModuleRef {
		t.Errorf("the job carries %q and the column holds %q", byID(j1.ID).ModuleRef, col)
	}
	// a replay of Start with another reference changes nothing the job carries (the first reference stands)
	if _, err := e.jobs.Start(ctx, j1.ID, "mod-job-OTHER"); err != nil {
		t.Fatal(err)
	}
	if got := byID(j1.ID).ModuleRef; got != "mod-job-carried" {
		t.Errorf("after a replay of Start with another reference the job carries %q", got)
	}

	// the controls: a job started with NO reference shows "" (the column is NULL: the read COALESCEs it), and a second job shows its own reference and nothing of the first
	j2 := e.running(u2, "m2", 2)
	if got := byID(j2.ID).ModuleRef; got != "" {
		t.Errorf("EngineJobByID of a job started with no reference: ModuleRef %q, want none", got)
	}
	if got := inWork(j2.ID).ModuleRef; got != "" {
		t.Errorf("EngineWork of a job started with no reference: ModuleRef %q, want none", got)
	}
	if !ledgerBool(t, e.admin, "SELECT module_job_ref IS NULL FROM platform.jobs WHERE id = $1", j2.ID) {
		t.Fatal("the fixture: the job started with no reference must hold NULL, which is what the read has to turn into an empty string")
	}
	j3 := e.confirmN(u3, "m1", 2)
	if _, err := e.jobs.Start(ctx, j3.ID, "mod-job-third"); err != nil {
		t.Fatal(err)
	}
	if got := byID(j3.ID).ModuleRef; got != "mod-job-third" {
		t.Errorf("the third job carries %q, want its own reference", got)
	}
	if got := inWork(j1.ID).ModuleRef; got != "mod-job-carried" {
		t.Errorf("the first job carries %q after another job started, want its own", got)
	}
}

// ONE call names every row of the largest job: module 1 takes a job's whole list in one request, so PendingRows, MarkSubmitted and EnsureModuleItems accept as many ids as a job may hold
// (jobs.MaxEngineIDs = jobs.DefaultMaxRows = 100000; it was 20000 and a module 1 job above that could never start) and refuse one more, typed, writing nothing. A job of 100000 rows is too heavy for CI,
// so this proves the limits on a small job with a call that names 100000 ids: the statement takes the whole list in one go and the rows named are the ones stamped.
func TestJobsEngine_OneCallTakesTheWholeListOfTheLargestJobAndNotOneMoreRow(t *testing.T) {
	e := newJeEnv(t)
	ctx := testCtx(t)
	if call, job := jobs.MaxEngineIDs, jobs.DefaultMaxRows; call != job || call != 100000 {
		t.Fatalf("MaxEngineIDs = %d, DefaultMaxRows = %d", call, job)
	}
	// ids no row has: canonical, distinct, and none of them a job row (job rows get random ids)
	filler := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1)
		}
		return out
	}

	j := e.running(e.newUser(), "m2", 5)
	// PendingRows: the limit is accepted at the maximum (the answer is the five rows there are) and refused one above it
	got, err := e.jobs.PendingRows(ctx, j.ID, jobs.MaxEngineIDs)
	if err != nil || len(got) != 5 {
		t.Fatalf("PendingRows(%d) = %d rows, %v; want the job's five rows (the limit is accepted)", jobs.MaxEngineIDs, len(got), err)
	}
	if _, err := e.jobs.PendingRows(ctx, j.ID, jobs.MaxEngineIDs+1); !errors.Is(err, jobs.ErrBadRequest) || !strings.Contains(err.Error(), fmt.Sprint(jobs.MaxEngineIDs)) {
		t.Errorf("PendingRows(%d) = %v, want ErrBadRequest naming the limit", jobs.MaxEngineIDs+1, err)
	}

	// MarkSubmitted: a call naming MaxEngineIDs ids (the job's five rows and filler) stamps exactly the five, in one statement, and a call naming one more is refused and stamps nothing
	ids := e.rowIDs(j.ID, "pending")
	tooMany := append(append([]string{}, ids...), filler(jobs.MaxEngineIDs-len(ids)+1)...)
	if len(tooMany) != jobs.MaxEngineIDs+1 {
		t.Fatalf("the fixture names %d ids, want %d", len(tooMany), jobs.MaxEngineIDs+1)
	}
	err = e.jobs.MarkSubmitted(ctx, j.ID, tooMany)
	if !errors.Is(err, jobs.ErrBadRequest) || !strings.Contains(err.Error(), fmt.Sprint(jobs.MaxEngineIDs+1)) {
		t.Fatalf("MarkSubmitted of %d ids = %v, want ErrBadRequest naming the count", len(tooMany), err)
	}
	if n := e.int("SELECT count(*) FROM platform.job_rows WHERE job_id = $1 AND submitted_at IS NOT NULL", j.ID); n != 0 {
		t.Fatalf("a refused call stamped %d rows", n)
	}
	if err := e.jobs.MarkSubmitted(ctx, j.ID, tooMany[:jobs.MaxEngineIDs]); err != nil {
		t.Fatalf("MarkSubmitted of %d ids (the maximum) = %v, want it accepted", jobs.MaxEngineIDs, err)
	}
	if n := e.int("SELECT count(*) FROM platform.job_rows WHERE job_id = $1 AND submitted_at IS NOT NULL", j.ID); n != 5 {
		t.Fatalf("the call at the maximum stamped %d rows, want the job's five", n)
	}
	if got, err := e.jobs.PendingRows(ctx, j.ID, jobs.MaxEngineIDs); err != nil || len(got) != 0 {
		t.Errorf("after the stamps PendingRows = %d rows, %v; want none", len(got), err)
	}

	// EnsureModuleItems takes the same number: one more is refused before anything is written, the maximum is accepted (it names rows that are not the job's, which is refused for that reason)
	s := e.running(e.newUser(), "m2", 2)
	_, err = e.jobs.EnsureModuleItems(ctx, s.ID, filler(jobs.MaxEngineIDs+1))
	if !errors.Is(err, jobs.ErrBadRequest) || !strings.Contains(err.Error(), fmt.Sprint(jobs.MaxEngineIDs+1)) {
		t.Errorf("EnsureModuleItems of %d ids = %v, want ErrBadRequest naming the count", jobs.MaxEngineIDs+1, err)
	}
	_, err = e.jobs.EnsureModuleItems(ctx, s.ID, filler(jobs.MaxEngineIDs))
	if !errors.Is(err, jobs.ErrBadRequest) || strings.Contains(err.Error(), "row ids in one call") {
		t.Errorf("EnsureModuleItems of %d ids (the maximum) = %v, want it past the count check and refused for the ids not being the job's rows", jobs.MaxEngineIDs, err)
	}
	if n := e.int("SELECT count(*) FROM public.job_items WHERE campaign_id = $1", s.ID); n != 0 {
		t.Errorf("a refused call wrote %d module items", n)
	}
}

// Every engine method answers an unknown job ErrNotFound and a job in the wrong state ErrNotAllowed with a reason, and NONE lets a raw database error out.
func TestJobsEngine_EveryMethodRefusesTheWrongJobAndTheWrongStateWithATypedError(t *testing.T) {
	e := newJeEnv(t)
	ctx := testCtx(t)
	draft, err := e.jobs.CreateDraft(ctx, e.newUser(), jobs.CreateReq{Module: "m2", Rows: jeAddrs(1, "w"), Content: jeContent()})
	if err != nil {
		t.Fatal(err)
	}
	rowID := e.rowIDs(draft.ID, "pending")[0]
	unknown := "00000000-0000-4000-8000-0000000000ee"
	calls := []struct {
		name  string
		do    func(id string) error
		draft string // the reason the draft job is refused with ("" for an answer of nil)
	}{
		{"Start", func(id string) error { _, err := e.jobs.Start(ctx, id, ""); return err }, "job_transition_illegal"},
		{"RecordResults", func(id string) error { _, err := e.jobs.RecordResults(ctx, id, nil); return err }, "not_settling"},
		{"Complete", func(id string) error { _, err := e.jobs.Complete(ctx, id); return err }, "job_transition_illegal"},
		{"Finish", func(id string) error { _, err := e.jobs.Finish(ctx, id); return err }, "not_completing"},
		{"Expire", func(id string) error { _, err := e.jobs.Expire(ctx, id); return err }, "job_transition_illegal"},
		{"FailJob", func(id string) error { _, err := e.jobs.FailJob(ctx, id, "x"); return err }, "job_transition_illegal"},
		{"Release", func(id string) error { return e.jobs.Release(ctx, id) }, "not_ended"},
		{"NoteProgress", func(id string) error { return e.jobs.NoteProgress(ctx, id) }, "not_active"},
		{"CancelUnsubmitted", func(id string) error { _, err := e.jobs.CancelUnsubmitted(ctx, id); return err }, "not_cancelling"},
		{"EnsureModuleItems", func(id string) error { _, err := e.jobs.EnsureModuleItems(ctx, id, []string{rowID}); return err }, "not_submittable"},
		{"MarkSubmitted", func(id string) error { return e.jobs.MarkSubmitted(ctx, id, []string{rowID}) }, "not_submittable"},
		{"AlertStale", func(id string) error { _, err := e.jobs.AlertStale(ctx, id, time.Hour, nil); return err }, ""},
		{"Flag", func(id string) error { return e.jobs.Flag(ctx, id, nil) }, ""},
		{"EngineJobByID", func(id string) error { _, err := e.jobs.EngineJobByID(ctx, id); return err }, ""},
	}
	for _, c := range calls {
		for _, id := range []string{unknown, "not-a-uuid", ""} {
			err := c.do(id)
			if !errors.Is(err, jobs.ErrNotFound) {
				t.Errorf("%s(%q): got %v, want ErrNotFound", c.name, id, err)
			}
			jeNoRawSQL(t, c.name, err)
		}
		err := c.do(draft.ID)
		if c.draft == "" {
			if err != nil {
				t.Errorf("%s on a draft: %v, want nil", c.name, err)
			}
			continue
		}
		jeNotAllowed(t, c.name+" on a draft job", err, c.draft)
	}
	if e.jobStatus(draft.ID) != jobs.StatusDraft || e.int("SELECT count(*) FROM platform.job_rows WHERE job_id = $1 AND submitted_at IS NOT NULL", draft.ID) != 0 {
		t.Fatal("a refused call changed the draft")
	}
}

func TestJobsEngine_NoteProgressAlertStaleAndFlagAreAnnotationsThatNeverMoveTheJob(t *testing.T) {
	e := newJeEnv(t)
	ctx := testCtx(t)
	u, u2 := e.newUser(), e.newUser()
	j := e.running(u, "m2", 2)

	p1 := e.str("SELECT last_progress_at::text FROM platform.jobs WHERE id = $1", j.ID)
	time.Sleep(30 * time.Millisecond)
	if err := e.jobs.NoteProgress(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	if p2 := e.str("SELECT last_progress_at::text FROM platform.jobs WHERE id = $1", j.ID); p2 == p1 {
		t.Fatal("NoteProgress did not move last_progress_at")
	}

	// ONE stale event per interval: the first call writes, the second inside the interval does not, and the job is not touched
	wrote, err := e.jobs.AlertStale(ctx, j.ID, time.Hour, map[string]any{"why": "x"})
	if err != nil || !wrote {
		t.Fatalf("AlertStale: %v, %v", wrote, err)
	}
	if again, err := e.jobs.AlertStale(ctx, j.ID, time.Hour, nil); err != nil || again {
		t.Fatalf("a second AlertStale inside the interval: %v, %v", again, err)
	}
	if e.eventCount(j.ID, "stale") != 1 || e.jobStatus(j.ID) != jobs.StatusRunning || !ledgerBool(t, e.admin, "SELECT stale_alerted_at IS NOT NULL FROM platform.jobs WHERE id = $1", j.ID) {
		t.Fatalf("%d stale events, status %s", e.eventCount(j.ID, "stale"), e.jobStatus(j.ID))
	}
	if got := e.str(`SELECT actor_kind || '|' || COALESCE(acting_user_id::text, '-') || '|' || (detail->>'why') FROM platform.job_events WHERE job_id = $1 AND kind = 'stale'`, j.ID); got != "engine|-|x" {
		t.Errorf("the stale event: %q", got)
	}
	// the control: with a short interval the next alert is due once the interval has passed
	time.Sleep(120 * time.Millisecond)
	if again, err := e.jobs.AlertStale(ctx, j.ID, 50*time.Millisecond, nil); err != nil || !again || e.eventCount(j.ID, "stale") != 2 {
		t.Fatalf("an alert after its interval: %v, %v, %d events", again, err, e.eventCount(j.ID, "stale"))
	}
	if _, err := e.jobs.AlertStale(ctx, j.ID, -time.Second, nil); !errors.Is(err, jobs.ErrBadRequest) {
		t.Errorf("a negative interval: %v", err)
	}

	// flags: the reason is the detail's when it has one
	if err := e.jobs.Flag(ctx, j.ID, map[string]any{"reason": "something is odd", "n": 2}); err != nil {
		t.Fatal(err)
	}
	if err := e.jobs.Flag(ctx, j.ID, nil); err != nil {
		t.Fatal(err)
	}
	flags := e.rows(`SELECT reason || '|' || actor_kind || '|' || COALESCE(acting_user_id::text, '-') || '|' || COALESCE(detail->>'n', '-') FROM platform.job_events WHERE job_id = $1 AND kind = 'flag' ORDER BY id`, j.ID)
	if len(flags) != 2 || flags[0] != "something is odd|engine|-|2" || flags[1] != "flagged by the engine|engine|-|-" {
		t.Fatalf("flags = %v", flags)
	}
	if e.jobStatus(j.ID) != jobs.StatusRunning {
		t.Fatal("an annotation moved the job")
	}

	// a job that holds nothing any more: no progress, no alert (not an error: it ended while the engine was looking)
	c := e.confirmN(u2, "m2", 1)
	if _, err := e.jobs.CancelBeforeStart(ctx, u2, c.ID); err != nil {
		t.Fatal(err)
	}
	jeNotAllowed(t, "NoteProgress of a cancelled job", e.jobs.NoteProgress(ctx, c.ID), "not_active")
	if wrote, err := e.jobs.AlertStale(ctx, c.ID, time.Hour, nil); err != nil || wrote || e.eventCount(c.ID, "stale") != 0 {
		t.Fatalf("AlertStale of a cancelled job: %v, %v", wrote, err)
	}
}

func TestJobsEngine_CancelUnsubmittedEndsOnlyTheRowsNoModuleWasGivenAndTheJobEndsCancelled(t *testing.T) {
	e := newJeEnv(t)
	ctx := testCtx(t)
	u := e.newUser()
	j := e.running(u, "m2", 4)
	ids := e.rowIDs(j.ID, "pending")
	if err := e.jobs.MarkSubmitted(ctx, j.ID, ids[:2]); err != nil {
		t.Fatal(err)
	}
	e.record(j.ID, jeItem(ids[0], jeM2Sent))

	_, err := e.jobs.CancelUnsubmitted(ctx, j.ID)
	jeNotAllowed(t, "CancelUnsubmitted of a running job", err, "not_cancelling")

	cj, err := e.jobs.CancelBeforeStart(ctx, u, j.ID) // the user's cancel of a running job: the request, not the end
	if err != nil || cj.Status != jobs.StatusCancelling || cj.CancelRequestedAt == nil {
		t.Fatalf("the cancel request: %+v, %v", cj.Status, err)
	}
	if rows, err := e.jobs.PendingRows(ctx, j.ID, 10); err != nil || len(rows) != 0 {
		t.Fatalf("a cancelling job still hands out rows: %v, %v", rows, err)
	}
	n, err := e.jobs.CancelUnsubmitted(ctx, j.ID)
	if err != nil || n != 2 {
		t.Fatalf("CancelUnsubmitted: %d, %v; want the two rows no module was given", n, err)
	}
	got := e.rows(`SELECT ordinal::text || '|' || origin || '|' || module_state || '|' || decision || '|' || rule FROM platform.job_results WHERE job_id = $1 ORDER BY ordinal`, j.ID)
	if want := []string{"1|module|sent|charge|M2-01", "3|platform|cancelled|free|P-01", "4|platform|cancelled|free|P-01"}; strings.Join(got, ";") != strings.Join(want, ";") {
		t.Fatalf("results = %v, want %v (the stamped row 2 has no result: the module holds it)", got, want)
	}
	before := e.fingerprint()
	if n, err := e.jobs.CancelUnsubmitted(ctx, j.ID); err != nil || n != 0 || e.fingerprint() != before {
		t.Fatalf("a replay: %d, %v", n, err)
	}
	// the held row is still unanswered, so the job cannot complete
	_, err = e.jobs.Complete(ctx, j.ID)
	var inc *jobs.IncompleteError
	if !errors.As(err, &inc) || inc.Missing != 1 {
		t.Fatalf("Complete with the held row unanswered: %v", err)
	}
	e.record(j.ID, jeItem(ids[1], jeM2Cancel)) // the module cancelled it: free
	if c, err := e.jobs.Complete(ctx, j.ID); err != nil || c.Status != jobs.StatusCompleting {
		t.Fatalf("Complete: %v, %v", c.Status, err)
	}
	fin, err := e.jobs.Finish(ctx, j.ID)
	if err != nil || fin.Status != jobs.StatusCancelled {
		t.Fatalf("Finish of a job whose cancel was accepted: %v, %v; want cancelled and never succeeded", fin.Status, err)
	}
	if e.transitionCount(j.ID, jobs.StatusSucceeded) != 0 || e.transitionCount(j.ID, jobs.StatusCancelled) != 1 {
		t.Fatal("the job did not end through completing -> cancelled")
	}
	if r, c, l := e.reserveOf(j.ID); r != 4 || c != 1 || l != 3 {
		t.Fatalf("reserve held %d, charged %d, released %d; want 4, 1 and 3", r, c, l)
	}
	e.requireEnded(j.ID, jobs.StatusCancelled, 1)
	e.requireBal("after the cancel", u, "m2", 999, 0)
}

// Every step that writes takes the (user, module) advisory lock FIRST: a call is PROVEN blocked behind a holder (a backend of the platform waiting on that very lock), is not done while it holds,
// and is done once the lock is let go.
func TestJobsEngine_EveryStepTakesTheUserModuleLockFirst(t *testing.T) {
	e := newJeEnv(t)
	ctx := testCtx(t)

	u1 := e.newUser()
	j1 := e.confirmN(u1, "m2", 2)
	if err := e.requireBlockedOnTheLock(u1, "m2", func(c context.Context) error { _, err := e.jobs.Start(c, j1.ID, ""); return err }); err != nil {
		t.Fatalf("Start after the lock was let go: %v", err)
	}
	if e.jobStatus(j1.ID) != jobs.StatusRunning {
		t.Fatalf("Start did not complete after the holder let go: %s", e.jobStatus(j1.ID))
	}

	u2 := e.newUser()
	j2 := e.running(u2, "m2", 2)
	ids := e.rowIDs(j2.ID, "pending")
	if err := e.requireBlockedOnTheLock(u2, "m2", func(c context.Context) error {
		_, err := e.jobs.RecordResults(c, j2.ID, []jobs.ResultItem{jeItem(ids[0], jeM2Sent)})
		return err
	}); err != nil {
		t.Fatalf("RecordResults after the lock was let go: %v", err)
	}
	if e.resultCount(j2.ID) != 1 || e.chargeRows(j2.ID) != 1 {
		t.Fatalf("RecordResults did not complete after the holder let go: %d results, %d charges", e.resultCount(j2.ID), e.chargeRows(j2.ID))
	}

	u3 := e.newUser()
	j3 := e.confirmN(u3, "m2", 2)
	jobWalkTo(t, e.admin, j3.ID, jobs.StatusFailed)
	if err := e.requireBlockedOnTheLock(u3, "m2", func(c context.Context) error { return e.jobs.Release(c, j3.ID) }); err != nil {
		t.Fatalf("Release after the lock was let go: %v", err)
	}
	if e.remainder(j3.ID) != 0 || e.releaseRows(j3.ID) != 1 {
		t.Fatal("Release did not complete after the holder let go")
	}

	// the control that the holder is what blocks: another user's call is not blocked by it
	u4 := e.newUser()
	j4 := e.confirmN(u4, "m2", 1)
	h := e.comHold(u1, "m2")
	if _, err := e.jobs.Start(ctx, j4.ID, ""); err != nil {
		t.Fatalf("a call for another user was blocked by the holder: %v", err)
	}
	h.release()
	e.requireBooks("after the lock tests")
}
