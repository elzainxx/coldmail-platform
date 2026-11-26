//go:build harness && m1node

package harness

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	m1ad "github.com/coldmail-platform/platform/internal/adapters/m1"
	"github.com/coldmail-platform/platform/internal/contractcheck"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// Phase 4, chunk D1, phase 1: module 1's side of the pending contracts (contract_pending_helpers_test.go says what a pending contract is and how a session treats it). The step runs inside the last
// test of the binary (TestM1Node_ZContractDrift, in a subtest of its own so that a failure of the step cannot hide the session's verdict), against the REAL module 1 with its real verifier node,
// through an adapter whose options carry a capture (not the session's observer), for one tenant:
//
//	POST /v1/jobs/{id}/cancel   `ok`         a RUNNING job of thirty rows that each wait out the fixture's 2 s DNS silence, cancelled at once: cancel_outcome `cancelled`, the unclaimed rows cancelled (free, no
//	                                         verdict), the rows already leased finishing and settling; and the same call again, `already_cancelled` (never an error); and, on a finished job that was never cancelled
//	                                         (two rows that answer at once), `already_complete` (nothing to cancel, the job is NOT marked cancelled). The three outcomes are one shape: one contract, one file, recorded once
//	                                         (the first answer, the outcome `cancelled`); the other two are compared with it in compare mode
//	GET  /v1/jobs/{id}          (existing)   the status of the cancelled job once it is complete: counts.cancelled and totals.cancelled above zero, cancel_requested true, submitted = settled + cancelled. It
//	                                         is the body of any job's status, so it is compared (compare mode) with the recording of the contract `complete`, and no contract of its own exists for it
//	GET  /v1/jobs/{id}/results  `page_cancelled`  the listing of the cancelled job in pages of six: the first page that holds a row cancelled before its check began (status `cancelled`, reason
//	                                         `cancelled_by_caller`, an empty `flags`) is the page to record; the others that hold one are compared
//	POST /v1/jobs/{id}/cancel   `not_found`  a job that does not exist: the 404 envelope, as the other routes' 404s are recorded
//
// Each call asserts what module 1 itself says (the outcome word, the counts and their arithmetic, the rows of the listing against the status) so that a step that recorded a no-op where it meant to
// cancel fails loudly. The controls are the repeats and the finished job; the job ends complete (every row settled or cancelled), so no row is left probing into the next test.
const (
	m1cpRows    = 30 // slow rows: the drainer leases ten per tick, so most are unclaimed at the cancel
	m1cpCancel  = "POST /v1/jobs/{id}/cancel"
	m1cpResults = "GET /v1/jobs/{id}/results"
	m1cpStatus  = "GET /v1/jobs/{id}"
)

// m1cpWaitComplete polls the job's Status through the step's adapter until module 1 says it is complete and returns that status.
func m1cpWaitComplete(t *testing.T, s *m1eStack, ad *m1ad.Adapter, jobID string) m1ad.JobStatus {
	t.Helper()
	deadline := time.Now().Add(m1eJobWait)
	for {
		st, err := ad.Status(s.ctx, jobID)
		if err != nil {
			t.Fatalf("Status of job %s failed (kind %v): %v", jobID, modclient.KindOf(err), err)
		}
		if st.Complete {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s was not complete after %v: status %q, total %d, settled %d, pending %d, cancelled %d", jobID, m1eJobWait, st.Status, st.Counts.Total, st.Counts.Settled, st.Counts.Pending, st.Counts.Cancelled)
		}
		time.Sleep(m1nPollEvery)
	}
}

func m1CancelCases(t *testing.T) {
	t.Helper()
	const step = "module 1: cancel of a running job, its repeats, a cancelled results page and an unknown job"
	cp := m1cp
	if !cp.anyArmed() {
		cp.noteSkipped(t, step)
		return
	}
	s := newM1eStack(t)
	tn := s.newTenant("cxl", true)
	capt := cp.capture()
	ad, err := m1ad.New(s.base, tn.scope.M1Key, nil, modclient.Options{Timeout: 60 * time.Second, Observe: capt.Observe})
	if err != nil {
		t.Fatalf("m1.New: %v", err) // never carries the URL or the key
	}

	// ---- a running job of thirty slow rows, cancelled at once ----
	silent := m1eRecipe(s.m1nEnv, "dns_silent_own")
	var slow []m1eItem
	for i := 0; i < m1cpRows; i++ {
		name := fmt.Sprintf("cxl%02d.%s", i, m1oZone)
		s.fx.setMode(name, "silent")
		local := fresh("cxl")
		it := m1eNewRecipeItem(silent, local)
		it.address = local + "@" + name
		m1eAddItem(&slow, it)
	}
	inputs := make([]m1ad.Input, len(slow))
	for i, it := range slow {
		inputs[i] = m1ad.Input{ItemRef: it.ref, Address: it.address}
	}
	sub, err := ad.Submit(s.ctx, m1ad.Prepare(inputs), m1eKey("cxl-"+tn.name))
	if err != nil {
		t.Fatalf("Submit of the thirty slow rows failed (kind %v): %v", modclient.KindOf(err), err)
	}
	jobID := sub.JobID
	t.Logf("NOTE m1 cxl setup: tenant %s, job %s of %d rows that each wait out the fixture's 2 s DNS silence", tn.userID, jobID, m1cpRows)

	res, err := ad.Cancel(s.ctx, jobID)
	o := capt.answer(t, "POST /v1/jobs/{id}/cancel of a running job", http.StatusOK)
	if err != nil || res.Outcome != m1ad.CancelOutcomeCancelled || !res.Status.CancelRequested || res.Status.Counts.Total != m1cpRows || res.Status.Counts.Cancelled < 1 {
		t.Fatalf("Cancel of the running job answered %+v (error %v), want outcome %s, cancel_requested, %d rows and at least one cancelled (the drainer leases ten rows a tick: the rest were unclaimed)", res, err, m1ad.CancelOutcomeCancelled, m1cpRows)
	}
	cp.judge(t, cpKey(m1cpCancel, "ok"), o, true)
	t.Logf("NOTE m1 cxl cancel: the first cancel answered %s (status %s, %d of %d rows cancelled, %d settled, %d pending)", res.Outcome, res.Status.Status, res.Status.Counts.Cancelled, res.Status.Counts.Total, res.Status.Counts.Settled, res.Status.Counts.Pending)

	again, err := ad.Cancel(s.ctx, jobID)
	o2 := capt.answer(t, "POST /v1/jobs/{id}/cancel of the same job again", http.StatusOK)
	if err != nil || again.Outcome != m1ad.CancelOutcomeAlreadyCancelled || !again.Status.CancelRequested || again.Status.Counts.Total != m1cpRows {
		t.Fatalf("the repeated Cancel answered %+v (error %v), want outcome %s: a repeat is never an error and changes nothing", again, err, m1ad.CancelOutcomeAlreadyCancelled)
	}
	cp.judge(t, cpKey(m1cpCancel, "ok"), o2, false)

	// ---- the job completes once the rows in flight have settled; the status says what the cancel did ----
	st := m1cpWaitComplete(t, s, ad, jobID)
	o3 := capt.answer(t, "GET /v1/jobs/{id} of the cancelled job", http.StatusOK)
	if st.Counts.Total != m1cpRows || st.Counts.Cancelled < 1 || st.Counts.Pending != 0 || st.Counts.Settled+st.Counts.Cancelled != st.Counts.Total || !st.CancelRequested ||
		st.Totals.Cancelled != st.Counts.Cancelled || st.Totals.Submitted != st.Totals.Settled+st.Totals.Cancelled || st.Totals.Pending != 0 {
		t.Fatalf("the status of the cancelled job is %+v, want complete with every row settled or cancelled (submitted = settled + cancelled, pending 0), cancel_requested and counts.cancelled = totals.cancelled above zero", st)
	}
	if !cp.switchOn && !cp.rec.Recording() {
		// the status of a job after a cancel is the body of any job's status (counts.cancelled, totals.cancelled and cancel_requested are keys of the recordings of the contracts `running` and
		// `complete`): it is compared with the recording of the one it belongs to, outside the session (the session never sees these calls while the contracts are pending)
		var rc modclient.RouteContract
		for _, c := range m1ad.Contracts() {
			if c.Route == m1cpStatus && c.Case == "complete" {
				rc = c
			}
		}
		if rc.Fixture == "" {
			t.Fatalf("module 1's contracts have no %s / complete: the status of a cancelled job has nothing to be compared with", m1cpStatus)
		}
		contractcheck.AssertLive(t, rc, m1ad.ContractFixtures(), "testdata/contract", o3.Status, o3.Body, nil)
	}
	t.Logf("NOTE m1 cxl status: the cancelled job is complete: %d rows, %d settled, %d cancelled, cancel_requested %v", st.Counts.Total, st.Counts.Settled, st.Counts.Cancelled, st.CancelRequested)

	// ---- the listing: pages of six, every row once; the cancelled rows are the status's count ----
	var all []m1ad.Row
	recorded, cursor := false, ""
	for pages := 1; ; pages++ {
		rows, next, err := ad.Results(s.ctx, jobID, cursor, 6)
		op := capt.answer(t, fmt.Sprintf("GET /v1/jobs/{id}/results, page %d", pages), http.StatusOK)
		if err != nil {
			t.Fatalf("Results page %d of the cancelled job failed (kind %v): %v", pages, modclient.KindOf(err), err)
		}
		for _, r := range rows {
			if r.Status == m1ad.StatusCancelled {
				cp.judge(t, cpKey(m1cpResults, "page_cancelled"), op, !recorded) // the first page that holds a cancelled row is the one to record
				recorded = true
				break
			}
		}
		all = append(all, rows...)
		if next == "" {
			break
		}
		if pages > 2*m1cpRows {
			t.Fatalf("the listing of the cancelled job did not end after %d pages", pages)
		}
		cursor = next
	}
	cancelled, verdicts := 0, 0
	for _, r := range all {
		switch {
		case r.Status == m1ad.StatusCancelled && r.Reason == m1ad.ReasonCancelledByCaller && !r.FromCache:
			cancelled++
		case r.Status != m1ad.StatusCancelled:
			verdicts++
		default:
			t.Errorf("row %d is cancelled with the reason %q and from_cache %v, want the reason %s and no cache signal", r.Ordinal, r.Reason, r.FromCache, m1ad.ReasonCancelledByCaller)
		}
	}
	if !recorded || len(all) != m1cpRows || int64(cancelled) != int64(st.Counts.Cancelled) || int64(verdicts) != int64(st.Counts.Settled) {
		t.Fatalf("the listing holds %d row(s): %d cancelled and %d with a verdict (a page that holds a cancelled row was seen: %v); the status says %d rows, %d cancelled, %d settled", len(all), cancelled, verdicts, recorded, st.Counts.Total, st.Counts.Cancelled, st.Counts.Settled)
	}
	t.Logf("NOTE m1 cxl results: %d rows in pages of six, %d cancelled (reason %s, no verdict) and %d settled, as the status says", len(all), cancelled, m1ad.ReasonCancelledByCaller, verdicts)

	// ---- a finished job that was never cancelled: already_complete ----
	nomx := m1eRecipe(s.m1nEnv, "no_mx").domain
	fast := m1xRows("cxf", nomx, 2)
	for i := range fast {
		fast[i].Address = fresh("cxf") + "@" + nomx
	}
	fsub, err := ad.SubmitRaw(s.ctx, fast, m1eKey("cxf-"+tn.name))
	if err != nil {
		t.Fatalf("SubmitRaw of the two quick rows failed (kind %v): %v", modclient.KindOf(err), err)
	}
	fst := m1cpWaitComplete(t, s, ad, fsub.JobID)
	if fst.Counts.Total != len(fast) || fst.Counts.Settled != len(fast) || fst.Counts.Cancelled != 0 || fst.CancelRequested {
		t.Fatalf("the quick job is %+v, want complete with both rows settled, none cancelled and no cancel requested", fst)
	}
	done, err := ad.Cancel(s.ctx, fsub.JobID)
	o4 := capt.answer(t, "POST /v1/jobs/{id}/cancel of a finished job", http.StatusOK)
	if err != nil || done.Outcome != m1ad.CancelOutcomeAlreadyComplete || done.Status.CancelRequested || done.Status.Counts.Cancelled != 0 || !done.Status.Complete {
		t.Fatalf("Cancel of a finished job answered %+v (error %v), want outcome %s, the job not marked cancelled and no row cancelled", done, err, m1ad.CancelOutcomeAlreadyComplete)
	}
	cp.judge(t, cpKey(m1cpCancel, "ok"), o4, false)

	// ---- a job that does not exist: the 404 envelope ----
	unknown, err := s.freshUUID()
	if err != nil {
		t.Fatalf("gen_random_uuid: %v", err)
	}
	_, err = ad.Cancel(s.ctx, unknown)
	o5 := capt.answer(t, "POST /v1/jobs/{id}/cancel of an unknown job", http.StatusNotFound)
	if _, p := m1eWantErr("Cancel of an unknown job", err, modclient.NotFound, http.StatusNotFound, "not_found"); len(p) != 0 {
		t.Errorf("Cancel of a job that does not exist: %v", p)
	}
	cp.judge(t, cpKey(m1cpCancel, "not_found"), o5, true)
	t.Logf("NOTE m1 cxl end: the cancelled job and the quick job are complete; the cancel of an unknown job is the 404 not_found of an absent job")
}

// m1cp is the pending contracts of module 1's session: the three answers above, declared by the adapter package whether or not their recordings are installed.
var m1cp = newCPSet("m1", m1ad.CancelContracts(), m1ad.ContractFixtures(), "testdata/contract", m1ad.RecordedPauseFixtures(), m1Drift)
