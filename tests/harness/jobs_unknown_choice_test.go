//go:build harness

package harness

import (
	"errors"
	"reflect"
	"testing"

	"github.com/coldmail-platform/platform/internal/jobs"
)

// OWNER RULING OD-4 (Phase 4 close-out, rule D3): the `unknown` choice of a chained send is explicit, never defaulted. The locked decision says the USER chooses; the store used to write `exclude` when
// nothing was said. Now, when the chained job's SOURCE verify job has at least one result whose verdict is `unknown`:
//
//	the confirm MUST carry include or exclude (ErrUnknownChoiceRequired, naming how many results are unknown), and a refusal writes nothing: no reserve, the job is still previewed
//	the choice must be the one the job was previewed under (a stale preview with the reason unknown_choice_changed), so nobody confirms a list under a choice they did not see
//	the preview shows unknown_count, the recorded choice (none yet: empty) and whether a choice is required, and PreviewUnknownChoice records it and reclassifies the unknown rows (pending for include,
//	excluded with their free P-EXC result at the confirm for exclude)
//
// A source with no unknown result needs no choice (a choice is then accepted and changes nothing), and a job that is not chained has none of this (a choice on it is a bad request). Every refusal has
// its twin; the database half (that the settings and the rows change only while the job is draft or previewed) is the lifecycle's and the rows guard's, tested elsewhere.

var jbsUnknownVerdicts = []string{"valid", "valid", "risky", "unknown", "invalid", "unknown"} // v1..v6: two unknown results, v4 and v6

func TestJobsStore_AChainWithUnknownResultsCannotBeConfirmedWithoutAnExplicitChoiceAndNothingIsWritten(t *testing.T) {
	e := newJbsEnv(t)
	A, B, C := mnyUserA, mnyUserB, mnyUserC
	for _, u := range []string{A, B, C} {
		e.fund(u, "m2", 100)
	}
	srcA, srcB := e.verifiedSource(A, jbsUnknownVerdicts...), e.verifiedSource(B, jbsUnknownVerdicts...)

	// NOTHING SAID: the confirm is refused with the count of unknown results, and nothing is written
	ja := e.chainOf(A, srcA.ID, "")
	pa := e.preview(A, ja.ID)
	if pa.UnknownCount != 2 || !pa.UnknownChoiceRequired || pa.UnknownChoice != "" || pa.Selected != 3 || pa.Units != 3 {
		t.Fatalf("the preview of a chain with unknown results and no choice is %+v, want 2 unknown, a choice required, none recorded, the unknown rows out (3 selected)", pa)
	}
	before := e.writes()
	_, err := e.tryConfirm(A, ja.ID, pa.Fingerprint, e.key(), nil)
	var need *jobs.UnknownChoiceRequiredError
	if !errors.Is(err, jobs.ErrUnknownChoiceRequired) || !errors.As(err, &need) || need.Unknown != 2 {
		t.Fatalf("a confirm with no choice: got %v, want ErrUnknownChoiceRequired naming 2 unknown results", err)
	}
	if errors.Is(err, jobs.ErrStalePreview) {
		t.Errorf("a missing choice is reported as a stale preview: %v", err)
	}
	e.requireWrites("a confirm with no choice for the unknown verdicts", before)
	if e.jobStatus(ja.ID) != "previewed" || e.reserveRows(ja.ID) != 0 || len(e.frozen(ja.ID)) != 0 || len(e.ruleCounts(ja.ID)) != 0 {
		t.Errorf("the refusal left the job %s with %d reserve rows, snapshot %v, results %v", e.jobStatus(ja.ID), e.reserveRows(ja.ID), e.frozen(ja.ID), e.ruleCounts(ja.ID))
	}
	e.requireBal("nothing was held", A, "m2", 100, 0)

	// A CHOICE RECORDED AT CREATE IS NOT THE CONFIRM'S CHOICE: the user states it again at the confirm, so the confirm cannot be a default either
	jb := e.chainOf(B, srcB.ID, jobs.UnknownInclude)
	pb := e.preview(B, jb.ID)
	if pb.UnknownChoice != jobs.UnknownInclude || !pb.UnknownChoiceRequired || pb.Pending != 5 || pb.Selected != 5 || pb.Credits != 5 {
		t.Fatalf("the preview of the chain created with include is %+v, want include recorded, still required, 5 pending, 5 credits", pb)
	}
	_, err = e.tryConfirm(B, jb.ID, pb.Fingerprint, e.key(), nil)
	jbsIs(t, "a confirm with no choice of a job created with include", err, jobs.ErrUnknownChoiceRequired)

	// EXCLUDE (user A): preview under it, confirm with it: the unknown rows are excluded with their free P-EXC results and are never handed to the module
	pe := e.previewChoice(A, ja.ID, jobs.UnknownExclude)
	ra, err := e.tryConfirmChoice(A, ja.ID, pe.Fingerprint, e.key(), jobs.UnknownExclude)
	mnyNoErr(t, "the confirm with exclude", err)
	if ra.Pending != 3 || ra.Reserved != 3 || ra.Job.Status != jobs.StatusConfirmed || ra.Job.Settings.UnknownChoice != jobs.UnknownExclude {
		t.Errorf("exclude: pending %d reserved %d status %s choice %q, want 3, 3, confirmed, exclude", ra.Pending, ra.Reserved, ra.Job.Status, ra.Job.Settings.UnknownChoice)
	}
	if got := e.rows("SELECT ordinal || ':' || module_state || ':' || COALESCE(reason, '') || ':' || rule || ':' || decision FROM platform.job_results WHERE job_id = $1 ORDER BY ordinal", ja.ID); !reflect.DeepEqual(got, []string{"4:excluded:unknown:P-EXC:free", "5:excluded:invalid:P-EXC:free", "6:excluded:unknown:P-EXC:free"}) {
		t.Errorf("the results of the exclude job are %v, want the two unknown rows and the invalid one, excluded, free, P-EXC", got)
	}
	pendA := e.rows("SELECT id::text FROM platform.job_rows WHERE job_id = $1 AND state = 'pending' ORDER BY ordinal", ja.ID)
	if n, err := e.jobs.EnsureModuleItems(testCtx(t), ja.ID, pendA); err != nil || n != 3 {
		t.Fatalf("EnsureModuleItems(exclude) = %d, %v", n, err)
	}
	if got, want := e.jobItemEmails(ja.ID), []string{"v1@example.test", "v2@example.test", "v3@example.test"}; !reflect.DeepEqual(got, want) {
		t.Errorf("the module was handed %v under exclude, want %v", got, want)
	}

	// INCLUDE (user B): the unknown rows are pending and are the module's, the invalid one is not
	rb, err := e.tryConfirmChoice(B, jb.ID, pb.Fingerprint, e.key(), jobs.UnknownInclude)
	mnyNoErr(t, "the confirm with include", err)
	if rb.Pending != 5 || rb.Reserved != 5 || rb.Job.Settings.UnknownChoice != jobs.UnknownInclude {
		t.Errorf("include: pending %d reserved %d choice %q, want 5, 5, include", rb.Pending, rb.Reserved, rb.Job.Settings.UnknownChoice)
	}
	if got := e.rows("SELECT ordinal || ':' || rule FROM platform.job_results WHERE job_id = $1 ORDER BY ordinal", jb.ID); !reflect.DeepEqual(got, []string{"5:P-EXC"}) {
		t.Errorf("the results of the include job are %v, want only the invalid row's P-EXC", got)
	}
	pendB := e.rows("SELECT id::text FROM platform.job_rows WHERE job_id = $1 AND state = 'pending' ORDER BY ordinal", jb.ID)
	if n, err := e.jobs.EnsureModuleItems(testCtx(t), jb.ID, pendB); err != nil || n != 5 {
		t.Fatalf("EnsureModuleItems(include) = %d, %v", n, err)
	}
	if got, want := e.jobItemEmails(jb.ID), []string{"v1@example.test", "v2@example.test", "v3@example.test", "v4@example.test", "v6@example.test"}; !reflect.DeepEqual(got, want) {
		t.Errorf("the module was handed %v under include, want %v", got, want)
	}

	// THE CONTROL: a source with NO unknown result confirms with no choice at all (the default is not missed: nothing is asked)
	srcC := e.verifiedSource(C, "valid", "risky", "invalid")
	jc := e.chainOf(C, srcC.ID, "")
	pc := e.preview(C, jc.ID)
	if pc.UnknownCount != 0 || pc.UnknownChoiceRequired || pc.UnknownChoice != "" || pc.Selected != 2 {
		t.Fatalf("the preview of a chain with no unknown result is %+v, want 0 unknown, no choice required, none recorded", pc)
	}
	rc, err := e.tryConfirm(C, jc.ID, pc.Fingerprint, e.key(), nil)
	mnyNoErr(t, "the confirm of a chain with no unknown result and no choice", err)
	if rc.Pending != 2 || rc.Reserved != 2 || e.ruleCounts(jc.ID)[jobs.RuleExcluded] != 1 {
		t.Errorf("no unknown result: pending %d reserved %d results %v, want 2, 2 and one %s", rc.Pending, rc.Reserved, e.ruleCounts(jc.ID), jobs.RuleExcluded)
	}
	e.requireBooks("after the three chained confirms")
}

func TestJobsStore_AChoiceWithNoUnknownResultIsAcceptedAndChangesNothing(t *testing.T) {
	e := newJbsEnv(t)
	D, E := e.newUser(), e.newUser()
	for _, u := range []string{D, E} {
		e.fund(u, "m2", 100)
	}
	srcD, srcE := e.verifiedSource(D, "valid", "risky", "invalid"), e.verifiedSource(E, "valid", "risky", "invalid")

	// a choice given at confirm with no unknown result: accepted, and the job is the one the preview showed
	jd := e.chainOf(D, srcD.ID, "")
	pd := e.preview(D, jd.ID)
	rd, err := e.tryConfirmChoice(D, jd.ID, pd.Fingerprint, e.key(), jobs.UnknownInclude)
	mnyNoErr(t, "a confirm with a choice and no unknown result", err)
	if rd.Pending != 2 || rd.Reserved != 2 {
		t.Errorf("pending %d reserved %d, want 2, 2 (the choice changed nothing)", rd.Pending, rd.Reserved)
	}
	// a choice recorded at create or at preview with no unknown result: recorded, harmless, and the numbers are the same as without it
	je := e.chainOf(E, srcE.ID, jobs.UnknownInclude)
	pe := e.preview(E, je.ID)
	if pe.UnknownChoice != jobs.UnknownInclude || pe.UnknownCount != 0 || pe.UnknownChoiceRequired || pe.Pending != pd.Pending || pe.Credits != pd.Credits {
		t.Errorf("the preview with a recorded choice and no unknown result is %+v, want the numbers of the one without (%d pending, %d credits)", pe, pd.Pending, pd.Credits)
	}
	if re, err := e.tryConfirmChoice(E, je.ID, pe.Fingerprint, e.key(), jobs.UnknownInclude); err != nil || re.Pending != 2 {
		t.Errorf("the confirm of the job with a recorded choice: %+v, %v", re, err)
	}
}

func TestJobsStore_AChoiceChangedBetweenPreviewAndConfirmIsStaleAndNobodyConfirmsAListTheyDidNotSee(t *testing.T) {
	e := newJbsEnv(t)
	A := mnyUserA
	e.fund(A, "m2", 100)
	src := e.verifiedSource(A, jbsUnknownVerdicts...)
	j := e.chainOf(A, src.ID, "")
	pInc := e.previewChoice(A, j.ID, jobs.UnknownInclude)
	pExc := e.previewChoice(A, j.ID, jobs.UnknownExclude) // another tab changes the job under the first one
	if pInc.Fingerprint == pExc.Fingerprint || pInc.Pending != 5 || pExc.Pending != 3 {
		t.Fatalf("the two previews: include %+v, exclude %+v, want two fingerprints, 5 pending and 3 pending", pInc, pExc)
	}

	stale := func(what string, err error) {
		t.Helper()
		var sp *jobs.StalePreviewError
		if !errors.Is(err, jobs.ErrStalePreview) || !errors.As(err, &sp) || sp.Reason != jobs.ReasonUnknownChoiceChanged || errors.Is(err, jobs.ErrUnknownChoiceRequired) {
			t.Fatalf("%s: got %v, want a stale preview with the reason %s", what, err, jobs.ReasonUnknownChoiceChanged)
		}
	}
	before := e.writes()
	// the user saw include, the job is now excluded: the confirm of the include list is refused for the choice (with either fingerprint: the choice is judged first, so the refusal names its cause)
	_, err := e.tryConfirmChoice(A, j.ID, pExc.Fingerprint, e.key(), jobs.UnknownInclude)
	stale("include with the fingerprint of the exclude preview", err)
	_, err = e.tryConfirmChoice(A, j.ID, pInc.Fingerprint, e.key(), jobs.UnknownInclude)
	stale("include with the fingerprint of the include preview", err)
	e.requireWrites("a confirm under a choice the job was not previewed under", before)
	if e.jobStatus(j.ID) != "previewed" || e.reserveRows(j.ID) != 0 {
		t.Errorf("the refusals left the job %s with %d reserve rows", e.jobStatus(j.ID), e.reserveRows(j.ID))
	}
	// the three refusals are three different answers: no choice at all is the missing choice, not a stale one
	_, err = e.tryConfirm(A, j.ID, pExc.Fingerprint, e.key(), nil)
	jbsIs(t, "no choice", err, jobs.ErrUnknownChoiceRequired)

	// a stale preview with no reason of its own (a price changed) is the bare ErrStalePreview: the family says why only when the store knows
	e.exec("UPDATE platform.unit_costs SET cost = 2, changed_by = $1, change_reason = 'choice test' WHERE unit = 'm2_accepted'", mnyAdminA)
	_, err = e.tryConfirmChoice(A, j.ID, pExc.Fingerprint, e.key(), jobs.UnknownExclude)
	var sp *jobs.StalePreviewError
	if !errors.Is(err, jobs.ErrStalePreview) || errors.As(err, &sp) {
		t.Fatalf("a confirm after a price change: got %v, want the bare ErrStalePreview with no reason of its own", err)
	}
	e.exec("UPDATE platform.unit_costs SET cost = 1, changed_by = $1, change_reason = 'choice test, back' WHERE unit = 'm2_accepted'", mnyAdminA)

	// THE TWIN: preview under the choice the user wants and confirm with it
	p := e.previewChoice(A, j.ID, jobs.UnknownInclude)
	r, err := e.tryConfirmChoice(A, j.ID, p.Fingerprint, e.key(), jobs.UnknownInclude)
	mnyNoErr(t, "the confirm under the choice the job was previewed under", err)
	if r.Pending != 5 || r.Reserved != 5 || r.Job.Settings.UnknownChoice != jobs.UnknownInclude {
		t.Errorf("pending %d reserved %d choice %q, want 5, 5, include", r.Pending, r.Reserved, r.Job.Settings.UnknownChoice)
	}
	// the confirm's own replay is still a replay, whatever choice the replay carries (the key names the confirm)
	again, err := e.jobs.Confirm(testCtx(t), A, jobs.ConfirmReq{JobID: j.ID, Fingerprint: p.Fingerprint, Key: r.Job.ConfirmKey})
	mnyNoErr(t, "the replay", err)
	if !again.Replayed || again.Reserved != 5 {
		t.Errorf("the replay is %+v, want a replay of the confirm that held 5", again)
	}
	e.requireBooks("after the choice tests")
}

func TestJobsStore_PreviewUnderAChoiceReclassifiesTheUnknownRowsBothWaysAndOnlyWhileTheJobCanChange(t *testing.T) {
	e := newJbsEnv(t)
	A, B := mnyUserA, mnyUserB
	e.fund(A, "m2", 100)
	e.fund(B, "m2", 100)
	srcA := e.verifiedSource(A, jbsUnknownVerdicts...)
	states := func(job string) []string { // ordinal:state:reason
		var out []string
		for _, r := range e.rowsOf(job) {
			out = append(out, string(rune('0'+r.Ordinal))+":"+r.State+":"+r.Reason)
		}
		return out
	}
	inc, exc := e.chainOf(A, srcA.ID, jobs.UnknownInclude), e.chainOf(A, srcA.ID, jobs.UnknownExclude) // what CreateDraft makes of each choice
	j := e.chainOf(A, srcA.ID, "")                                                                     // and the job that is switched
	wantExc := []string{"1:pending:", "2:pending:", "3:pending:", "4:excluded:unknown", "5:excluded:invalid", "6:excluded:unknown"}
	wantInc := []string{"1:pending:", "2:pending:", "3:pending:", "4:pending:", "5:excluded:invalid", "6:pending:"}
	if !reflect.DeepEqual(states(exc.ID), wantExc) || !reflect.DeepEqual(states(inc.ID), wantInc) || !reflect.DeepEqual(states(j.ID), wantExc) {
		t.Fatalf("the rows made at create: exclude %v, include %v, none %v; want %v, %v and %v (none reads as the safe exclude)", states(exc.ID), states(inc.ID), states(j.ID), wantExc, wantInc, wantExc)
	}

	// include: the unknown rows become pending, exactly the rows CreateDraft makes under include, and the choice is recorded in the frozen settings
	p1 := e.previewChoice(A, j.ID, jobs.UnknownInclude)
	if !reflect.DeepEqual(states(j.ID), wantInc) || p1.UnknownChoice != jobs.UnknownInclude || p1.Pending != 5 || p1.Excluded != 1 || p1.Selected != 5 || p1.Credits != 5 {
		t.Errorf("after the preview under include the rows are %v and the preview %+v, want %v, 5 pending, 1 excluded, 5 credits", states(j.ID), p1, wantInc)
	}
	if g, err := e.jobs.Get(testCtx(t), A, j.ID); err != nil || g.Settings.UnknownChoice != jobs.UnknownInclude || g.Status != jobs.StatusPreviewed {
		t.Errorf("the job after the preview: %+v, %v, want status previewed with the choice include recorded", g, err)
	}
	if got := e.str("SELECT frozen_settings #>> '{settings,unknown_choice}' FROM platform.jobs WHERE id = $1", j.ID); got != jobs.UnknownInclude {
		t.Errorf("the frozen settings hold the choice %q", got)
	}
	// the same choice again changes nothing (the fingerprint is a function of its inputs)
	if p1b := e.previewChoice(A, j.ID, jobs.UnknownInclude); p1b.Fingerprint != p1.Fingerprint || !reflect.DeepEqual(states(j.ID), wantInc) {
		t.Errorf("the same choice again changed the preview: %s then %s, rows %v", p1.Fingerprint, p1b.Fingerprint, states(j.ID))
	}
	// and back to exclude: the rows return to the states CreateDraft gives them, the reason is the verdict
	p2 := e.previewChoice(A, j.ID, jobs.UnknownExclude)
	if !reflect.DeepEqual(states(j.ID), wantExc) || p2.UnknownChoice != jobs.UnknownExclude || p2.Pending != 3 || p2.Excluded != 3 || p2.Fingerprint == p1.Fingerprint {
		t.Errorf("after the preview under exclude the rows are %v and the preview %+v, want %v, 3 pending, 3 excluded and another fingerprint than include's", states(j.ID), p2, wantExc)
	}
	// a preview that states no choice leaves the recorded one alone
	if p3 := e.preview(A, j.ID); p3.UnknownChoice != jobs.UnknownExclude || p3.Fingerprint != p2.Fingerprint {
		t.Errorf("a preview with no choice changed the job's choice: %+v", p3)
	}

	// the refusals, each with nothing written, and the twin that is accepted
	plain := e.create(B, "m2", 3)
	before := e.writes()
	_, err := e.jobs.Preview(testCtx(t), A, j.ID, jobs.PreviewUnknownChoice("maybe"))
	jbsBadRequest(t, "a choice outside the vocabulary", err, "unknown_choice")
	_, err = e.jobs.Preview(testCtx(t), A, j.ID, jobs.PreviewUnknownChoice(""))
	jbsBadRequest(t, "an empty choice (it is not 'no choice')", err, "unknown_choice")
	_, err = e.jobs.Preview(testCtx(t), B, plain.ID, jobs.PreviewUnknownChoice(jobs.UnknownExclude))
	jbsBadRequest(t, "a choice on a job that is not chained", err, "only applies to a send chained")
	e.requireWrites("the refused previews", before)
	if p := e.preview(B, plain.ID); p.UnknownChoice != "" || p.UnknownCount != 0 || p.UnknownChoiceRequired {
		t.Errorf("a job that is not chained previews as %+v, want no choice, none unknown, none required", p)
	}

	// a choice on a job that is not chained is a bad request at the confirm too (nothing written), and the confirm without one is the twin
	pp := e.preview(B, plain.ID)
	before = e.writes()
	_, err = e.tryConfirmChoice(B, plain.ID, pp.Fingerprint, e.key(), jobs.UnknownInclude)
	jbsBadRequest(t, "a choice at the confirm of a job that is not chained", err, "only applies to a send chained")
	_, err = e.tryConfirmChoice(B, plain.ID, pp.Fingerprint, e.key(), "maybe")
	jbsBadRequest(t, "a choice outside the vocabulary at the confirm", err, "unknown_choice")
	e.requireWrites("the refused confirms", before)
	if _, err := e.tryConfirm(B, plain.ID, pp.Fingerprint, e.key(), nil); err != nil {
		t.Errorf("the twin, the confirm of the job that is not chained with no choice: %v", err)
	}

	// once confirmed the job cannot be previewed under another choice: the rows do not move
	r, err := e.tryConfirmChoice(A, j.ID, p2.Fingerprint, e.key(), jobs.UnknownExclude)
	mnyNoErr(t, "the confirm", err)
	if r.Pending != 3 {
		t.Fatalf("pending %d, want 3", r.Pending)
	}
	_, err = e.jobs.Preview(testCtx(t), A, j.ID, jobs.PreviewUnknownChoice(jobs.UnknownInclude))
	jbsNotAllowed(t, "a preview of a confirmed job under another choice", err, "job_status_has_no_preview")
	if !reflect.DeepEqual(states(j.ID), wantExc) {
		t.Errorf("the rows of the confirmed job are %v after the refused preview, want %v", states(j.ID), wantExc)
	}
	if got := e.str("SELECT frozen_settings #>> '{settings,unknown_choice}' FROM platform.jobs WHERE id = $1", j.ID); got != jobs.UnknownExclude {
		t.Errorf("the confirmed job's frozen choice is %q, want exclude", got)
	}
	e.requireBooks("after the reclassification tests")
}

// ---- the live stack's confirm with the user's explicit choice (owner ruling OD-4) ----

// confirmChoice confirms a previewed job with the user's choice for the unknown verdicts of a chained send ("" says none) and returns the store's answer and error as they are.
func (s *jlStack) confirmChoice(user, job, fingerprint, choice string) (jobs.ConfirmResult, error) {
	s.t.Helper()
	return s.store.Confirm(s.ctx, user, jobs.ConfirmReq{JobID: job, Fingerprint: fingerprint, Key: jlKey("confirm"), UnknownChoice: choice})
}

// confirmChoiceOK is confirmChoice that fails the test on an error.
func (s *jlStack) confirmChoiceOK(user, job, fingerprint, choice string) jobs.ConfirmResult {
	s.t.Helper()
	res, err := s.confirmChoice(user, job, fingerprint, choice)
	if err != nil {
		s.t.Fatalf("Confirm(%s, choice %q): %v", jlShort(job), choice, err)
	}
	return res
}
