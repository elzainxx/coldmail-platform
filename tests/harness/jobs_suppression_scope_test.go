//go:build harness

package harness

import (
	"errors"
	"reflect"
	"testing"

	"github.com/coldmail-platform/platform/internal/jobs"
)

// OWNER RULING OD-3 (Phase 4 close-out, rule D2): the user's suppression list applies to SEND jobs only. Suppression exists so that nobody who unsubscribed is e-mailed; a verify job e-mails nobody.
//
//	send job (modules 2 and 3)   unchanged: counted at the preview, applied at the confirm, the row becomes state `suppressed`, its free platform result P-SUP is written, it is not reserved and it never
//	                             reaches the module
//	verify job (module 1)        the row STAYS `pending`, is verified and charged exactly like its unsuppressed twin, counts in the reserve and in the preview's credits, and carries the flag
//	                             `suppressed` into its results
//	verify -> send               the chained send takes the verify job's results as its rows and applies the user's list AGAIN at its own confirm: an address suppressed after the verify job finished
//	                             is not sent to, and the verify job's own result for it is not rewritten
//
// The flag is set by the confirm in the store (job_rows.suppressed) and derived into every result by the database (job_results_after_confirm): no writer states it. The database half (the CHECKs
// that tie the state `suppressed` to the flag, the guard that keeps a flagged pending row to a verify job, the derivation, the grants, the migration) is in job_rows_results_test.go and
// migrate_lifecycle_test.go. Every test pairs the verify job with its send twin or its unsuppressed twin.

func TestJobsStore_AVerifyJobVerifiesAndChargesItsSuppressedAddressesExactlyLikeAnUnsuppressedTwin(t *testing.T) {
	e := newJbsEnv(t)
	A, B := mnyUserA, mnyUserB // A suppresses two of the six addresses, B, the twin, none
	e.fund(A, "m1", 100)
	e.fund(B, "m1", 100)
	list := jbsAddrs(6, "r")
	ja, jb := e.createRows(A, "m1", list), e.createRows(B, "m1", list)
	e.suppressFor(A, "r00002@example.test", "r00005@example.test")

	// the preview counts them, says they will still be verified, and prices the whole list: the credits are the twin's
	pa, pb := e.preview(A, ja.ID), e.preview(B, jb.ID)
	if pa.Suppressed != 2 || !pa.SuppressedStillVerified || pa.Pending != 6 || pa.Units != 6 || pa.Credits != 6 || pa.Shortfall != 0 {
		t.Errorf("the verify preview with 2 suppressed addresses is %+v, want suppressed 2, still verified, 6 pending, 6 units, 6 credits", pa)
	}
	if pb.Suppressed != 0 || !pb.SuppressedStillVerified || pb.Units != 6 || pb.Credits != pa.Credits {
		t.Errorf("the twin's preview is %+v, want no suppressed address and the same 6 credits", pb)
	}

	// the confirm reserves for ALL six, flags two rows and leaves every row pending; no platform result is written for a row that will be verified
	ra, rb := e.confirm(A, ja.ID, pa.Fingerprint), e.confirm(B, jb.ID, pb.Fingerprint)
	if ra.Pending != 6 || ra.Reserved != 6 || ra.Suppressed != 2 || rb.Pending != 6 || rb.Reserved != 6 || rb.Suppressed != 0 {
		t.Errorf("confirm: suppressed job pending %d reserved %d flagged %d, twin pending %d reserved %d flagged %d; want 6, 6, 2 and 6, 6, 0", ra.Pending, ra.Reserved, ra.Suppressed, rb.Pending, rb.Reserved, rb.Suppressed)
	}
	if c := e.stateCounts(ja.ID); len(c) != 1 || c[jobs.StatePending] != 6 {
		t.Errorf("the verify job's rows are %v, want six rows, all pending (a suppressed row is not taken out of a verify job)", c)
	}
	if got := e.flaggedRows(ja.ID); !reflect.DeepEqual(got, []string{"2", "5"}) || len(e.flaggedRows(jb.ID)) != 0 {
		t.Errorf("the rows that carry the flag are %v (twin %v), want exactly rows 2 and 5 (twin none)", got, e.flaggedRows(jb.ID))
	}
	if len(e.ruleCounts(ja.ID)) != 0 {
		t.Errorf("the confirm wrote platform results %v for a list of six rows that all reach the module", e.ruleCounts(ja.ID))
	}
	for _, j := range []string{ja.ID, jb.ID} {
		if e.int("SELECT units FROM platform.ledger WHERE job_id = $1 AND kind = 'reserve'", j) != 6 || e.reserveRows(j) != 1 {
			t.Errorf("job %s: the reserve does not hold the 6 units of the whole list", j)
		}
	}

	// the module verifies both lists: the ledger charges the two jobs the same, unit by unit and credit by credit
	sa, sb := e.settleVerify(ja.ID), e.settleVerify(jb.ID)
	if sa.Charged != 6 || sa.Credits != 6 || sb.Charged != 6 || sb.Credits != 6 {
		t.Errorf("settlement: suppressed job %+v, twin %+v, want 6 charges of 6 credits each", sa, sb)
	}
	chargesA, chargesB := e.chargesByUnit(ja.ID), e.chargesByUnit(jb.ID)
	if !reflect.DeepEqual(chargesA, chargesB) || !reflect.DeepEqual(chargesA, []string{"m1_own:6:6"}) {
		t.Errorf("the ledger charges the suppressed job %v and its twin %v, want both exactly [m1_own:6:6]", chargesA, chargesB)
	}
	e.requireBal("the suppressed job's user paid for six rows", A, "m1", 94, 0)
	e.requireBal("the twin's user paid for six rows", B, "m1", 94, 0)

	// the results: all six are the module's, charged, and exactly the two suppressed addresses carry the flag (JSON through the store, and the table)
	if got := e.flaggedResults(ja.ID); !reflect.DeepEqual(got, []string{"2", "5"}) || len(e.flaggedResults(jb.ID)) != 0 {
		t.Errorf("the results that carry the flag are %v (twin %v), want exactly 2 and 5 (twin none)", got, e.flaggedResults(jb.ID))
	}
	res, err := e.jobs.Results(testCtx(t), A, ja.ID, 0, 100)
	mnyNoErr(t, "Results", err)
	if len(res) != 6 {
		t.Fatalf("%d results, want 6", len(res))
	}
	for _, r := range res {
		wantFlag := r.Ordinal == 2 || r.Ordinal == 5
		if r.Suppressed != wantFlag || r.Origin != "module" || r.State != "valid" || r.Decision != "charge" || r.Unit != "m1_own" {
			t.Errorf("result %d is %+v, want a charged module result flagged exactly for ordinals 2 and 5", r.Ordinal, r)
		}
	}
	twin, err := e.jobs.Results(testCtx(t), B, jb.ID, 0, 100)
	mnyNoErr(t, "Results (twin)", err)
	for _, r := range twin {
		if r.Suppressed {
			t.Errorf("the twin's result %d carries the flag", r.Ordinal)
		}
	}
	e.requireBooks("after the two verify jobs were settled")
}

func TestJobsStore_TheSameListAsASendJobLeavesTheSuppressedRowsOutFreeNotReservedAndNeverHandedToTheModule(t *testing.T) {
	e := newJbsEnv(t)
	C := mnyUserC
	e.fund(C, "m2", 100)
	j := e.createRows(C, "m2", jbsAddrs(6, "r"))
	e.suppressFor(C, "r00002@example.test", "r00005@example.test")

	p := e.preview(C, j.ID)
	if p.Pending != 6 || p.Suppressed != 2 || p.SuppressedStillVerified || p.Units != 4 || p.Credits != 4 {
		t.Errorf("the send preview with 2 suppressed addresses is %+v, want 6 pending, 2 suppressed, NOT still verified, 4 units, 4 credits", p)
	}
	r := e.confirm(C, j.ID, p.Fingerprint)
	if r.Pending != 4 || r.Suppressed != 2 || r.Reserved != 4 {
		t.Errorf("confirm: pending %d suppressed %d reserved %d, want 4, 2, 4", r.Pending, r.Suppressed, r.Reserved)
	}
	// the rows are state suppressed AND carry the flag (the database ties the two: a suppressed row without the flag cannot be written)
	if c := e.stateCounts(j.ID); c[jobs.StatePending] != 4 || c[jobs.StateSuppressed] != 2 || len(c) != 2 {
		t.Errorf("the rows are %v, want 4 pending and 2 suppressed", c)
	}
	if got := e.flaggedRows(j.ID); !reflect.DeepEqual(got, []string{"2", "5"}) {
		t.Errorf("the flagged rows are %v, want 2 and 5", got)
	}
	// free: a platform result P-SUP for each, flagged, and nothing charged; not reserved: the reserve holds the four
	want := []string{"2|platform|suppressed|free|P-SUP|true", "5|platform|suppressed|free|P-SUP|true"}
	if got := e.rows("SELECT ordinal, origin, module_state, decision, rule, suppressed FROM platform.job_results WHERE job_id = $1 ORDER BY ordinal", j.ID); !reflect.DeepEqual(got, want) {
		t.Errorf("the results are %v, want %v", got, want)
	}
	if e.int("SELECT units FROM platform.ledger WHERE job_id = $1 AND kind = 'reserve'", j.ID) != 4 || e.chargeRows(j.ID) != 0 {
		t.Errorf("the reserve does not hold exactly the four rows that reach the module, or something was charged")
	}

	// never handed to the module: the store refuses to write a message for a suppressed row, whoever asks (the control: the pending rows are accepted and are the only messages there are)
	ctx := testCtx(t)
	suppressed := e.rows("SELECT id::text FROM platform.job_rows WHERE job_id = $1 AND state = 'suppressed' ORDER BY ordinal", j.ID)
	pending := e.rows("SELECT id::text FROM platform.job_rows WHERE job_id = $1 AND state = 'pending' ORDER BY ordinal", j.ID)
	_, err := e.jobs.EnsureModuleItems(ctx, j.ID, suppressed[:1])
	jbsBadRequest(t, "a message for a suppressed row", err, "not pending rows")
	_, err = e.jobs.EnsureModuleItems(ctx, j.ID, append(append([]string{}, pending[:1]...), suppressed[1]))
	jbsBadRequest(t, "a message for a pending row and a suppressed one", err, "not pending rows")
	if got := e.jobItemEmails(j.ID); len(got) != 0 {
		t.Fatalf("a refused call left messages for %v", got)
	}
	n, err := e.jobs.EnsureModuleItems(ctx, j.ID, pending)
	mnyNoErr(t, "EnsureModuleItems (the four pending rows)", err)
	wantMail := []string{"r00001@example.test", "r00003@example.test", "r00004@example.test", "r00006@example.test"}
	if got := e.jobItemEmails(j.ID); n != 4 || !reflect.DeepEqual(got, wantMail) {
		t.Errorf("%d messages were written for %v, want the four addresses that are not suppressed %v", n, got, wantMail)
	}
	e.requireBooks("after the send job's confirm")
}

// The same job, a different module, the opposite of the first test: the verify job charges all six rows, the send job reserves four. Neither can be mistaken for the other because each is asserted
// against its own numbers, and the arithmetic of a shortfall is the sharpest place to see it.
func TestJobsStore_AVerifyJobsShortfallCountsItsSuppressedRowsToo(t *testing.T) {
	e := newJbsEnv(t)
	A := mnyUserA
	e.fund(A, "m1", 7)
	j, p := e.previewed(A, "m1", 10)
	if p.Credits != 10 || p.Available != 7 || p.Shortfall != 3 {
		t.Fatalf("the preview says %d credits, %d available, %d short; want 10, 7, 3", p.Credits, p.Available, p.Shortfall)
	}
	// a suppression after the preview does NOT lower what a verify job needs: ten rows are verified, so the shortfall stays 3 (a send job's would become 2: TestJobsStore_ShortfallIsExactAndNothingIsWritten)
	e.suppressFor(A, "r00001@example.test")
	before := e.writes()
	_, err := e.tryConfirm(A, j.ID, p.Fingerprint, e.key(), nil)
	var ic jobs.ErrInsufficientCredits
	if !errors.As(err, &ic) || ic.Available != 7 || ic.Needed != 10 || ic.Shortfall != 3 {
		t.Fatalf("a balance that covers part of a verify list: got %v, want ErrInsufficientCredits{available 7, needed 10, shortfall 3}", err)
	}
	e.requireWrites("a verify confirm the balance does not cover", before)
	if got := e.flaggedRows(j.ID); len(got) != 0 {
		t.Errorf("the refused confirm left the flag on rows %v: the flagging must roll back with the refusal", got)
	}
	// THE CONTROL: three more credits cover all ten rows, and the confirm flags the one suppressed row and holds the ten
	e.fund(A, "m1", 3)
	r, err := e.tryConfirm(A, j.ID, p.Fingerprint, e.key(), nil)
	mnyNoErr(t, "the confirm that fits", err)
	if r.Reserved != 10 || r.Pending != 10 || r.Suppressed != 1 {
		t.Errorf("confirm: reserved %d pending %d suppressed %d, want 10, 10, 1", r.Reserved, r.Pending, r.Suppressed)
	}
	if got := e.flaggedRows(j.ID); !reflect.DeepEqual(got, []string{"1"}) {
		t.Errorf("the flagged rows are %v, want row 1", got)
	}
	e.requireBal("the whole balance reserved", A, "m1", 0, 10)
	e.requireBooks("after the verify shortfall test")
}

// A verify job that is cancelled before it starts ends every row with the platform's `cancelled` result, from a writer that is not the engine's RecordResults: it still carries the flag, because the
// database derives it from the row (the one place), so no writer, present or future, can forget it or state it wrongly.
func TestJobsStore_EveryWriterOfAResultGetsTheFlagFromTheRow(t *testing.T) {
	e := newJbsEnv(t)
	A, B := mnyUserA, mnyUserB
	e.fund(A, "m1", 100)
	e.fund(B, "m1", 100)
	ja, jb := e.createRows(A, "m1", jbsAddrs(4, "r")), e.createRows(B, "m1", jbsAddrs(4, "r"))
	e.suppressFor(A, "r00001@example.test", "r00004@example.test")
	e.confirm(A, ja.ID, e.preview(A, ja.ID).Fingerprint)
	e.confirm(B, jb.ID, e.preview(B, jb.ID).Fingerprint) // the twin: the same list, nothing suppressed
	for _, c := range []struct{ user, job string }{{A, ja.ID}, {B, jb.ID}} {
		if _, err := e.jobs.CancelBeforeStart(testCtx(t), c.user, c.job); err != nil {
			t.Fatalf("CancelBeforeStart(%s): %v", c.job, err)
		}
	}
	for _, c := range []struct {
		job  string
		want []string
	}{{ja.ID, []string{"1", "4"}}, {jb.ID, nil}} {
		if got := e.rows("SELECT ordinal FROM platform.job_results WHERE job_id = $1 AND origin = 'platform' AND module_state = 'cancelled' AND suppressed ORDER BY ordinal", c.job); !reflect.DeepEqual(got, c.want) {
			t.Errorf("job %s: the cancelled results that carry the flag are %v, want %v", c.job, got, c.want)
		}
		if n := e.int("SELECT count(*) FROM platform.job_results WHERE job_id = $1 AND module_state = 'cancelled'", c.job); n != 4 {
			t.Errorf("job %s: %d cancelled results, want one per row (4)", c.job, n)
		}
	}
	e.requireBooks("after the two cancels")
}

// The chain: the verify job's result is flagged, the send takes the results as its rows and applies the user's list again, so an address suppressed AFTER the verify job finished is excluded from the send
// although the verify job's result for it (immutable) says it was not suppressed, and neither suppressed address reaches the module.
func TestJobsStore_VerifyThenSendStillExcludesTheSuppressedAddressesAtTheSend(t *testing.T) {
	e := newJbsEnv(t)
	A := mnyUserA
	e.fund(A, "m1", 100)
	e.fund(A, "m2", 100)
	ctx := testCtx(t)
	src := e.createRows(A, "m1", jbsAddrs(6, "r"))
	e.suppressFor(A, "r00002@example.test") // on the list BEFORE the verify job: it is verified, charged and flagged
	e.confirm(A, src.ID, e.preview(A, src.ID).Fingerprint)
	if s := e.settleVerify(src.ID); s.Charged != 6 {
		t.Fatalf("the verify job charged %d rows, want 6", s.Charged)
	}
	if _, err := e.jobs.Complete(ctx, src.ID); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if j, err := e.jobs.Finish(ctx, src.ID); err != nil || j.Status != jobs.StatusSucceeded {
		t.Fatalf("Finish: %v (%+v)", err, j)
	}
	if got := e.flaggedResults(src.ID); !reflect.DeepEqual(got, []string{"2"}) {
		t.Fatalf("the verify job's flagged results are %v, want ordinal 2 (a suppressed address shows suppressed: true there)", got)
	}

	// an address suppressed AFTER the verify job finished
	e.suppressFor(A, "r00004@example.test")
	s := e.chainOf(A, src.ID, "")
	ps := e.preview(A, s.ID)
	if ps.Rows != 6 || ps.Pending != 6 || ps.Suppressed != 2 || ps.SuppressedStillVerified || ps.Units != 4 || ps.Credits != 4 || ps.UnknownCount != 0 || ps.UnknownChoiceRequired {
		t.Fatalf("the chained send's preview is %+v, want 6 pending of which 2 are suppressed (r00002 and the new r00004), 4 units, 4 credits, nothing unknown", ps)
	}
	rs := e.confirm(A, s.ID, ps.Fingerprint)
	if rs.Pending != 4 || rs.Suppressed != 2 || rs.Reserved != 4 {
		t.Errorf("the chained send's confirm: pending %d suppressed %d reserved %d, want 4, 2, 4", rs.Pending, rs.Suppressed, rs.Reserved)
	}
	rows := e.rowsOf(s.ID)
	if rows[1].State != jobs.StateSuppressed || rows[3].State != jobs.StateSuppressed || e.stateCounts(s.ID)[jobs.StatePending] != 4 {
		t.Errorf("the chained send's rows 2 and 4 are %s and %s and %d are pending, want both suppressed and 4 pending", rows[1].State, rows[3].State, e.stateCounts(s.ID)[jobs.StatePending])
	}
	// neither reaches the module: the store refuses a message for either, and the four that are written are the other four
	for _, ord := range []int{1, 3} {
		if _, err := e.jobs.EnsureModuleItems(ctx, s.ID, []string{rows[ord].ID}); !errors.Is(err, jobs.ErrBadRequest) {
			t.Errorf("a message for the suppressed row %d: got %v, want ErrBadRequest", ord+1, err)
		}
	}
	var pend []string
	for _, r := range rows {
		if r.State == jobs.StatePending {
			pend = append(pend, r.ID)
		}
	}
	if n, err := e.jobs.EnsureModuleItems(ctx, s.ID, pend); err != nil || n != 4 {
		t.Fatalf("EnsureModuleItems(the four pending rows) = %d, %v", n, err)
	}
	if got, want := e.jobItemEmails(s.ID), []string{"r00001@example.test", "r00003@example.test", "r00005@example.test", "r00006@example.test"}; !reflect.DeepEqual(got, want) {
		t.Errorf("the module was handed %v, want %v: a suppressed address reached the sink", got, want)
	}
	// the verify job's results are immutable: r00004's result still says it was not suppressed when that job was confirmed
	if got := e.flaggedResults(src.ID); !reflect.DeepEqual(got, []string{"2"}) {
		t.Errorf("after the send, the verify job's flagged results are %v, want still only ordinal 2", got)
	}
	e.requireBooks("after the chained send")
}
