//go:build harness

package harness

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/coldmail-platform/platform/internal/jobs"
	"github.com/coldmail-platform/platform/internal/money"
)

// The job store (internal/jobs) against a REAL PostgreSQL: Phase 4, chunks B5 (create, classify, preview) and B6 (confirm in one transaction). Every test has a scratch database of its own, runs the
// REAL store as a real platform_app login over the real money store, and proves what it claims from the database (the tables, the ledger, pg_stat_activity), never from the store's own answer
// alone. Every refusal has its legal twin (the nearest request that is accepted) and every counting test a control that shows the comparison itself can fail. Helpers are in
// jobs_store_helpers_test.go (jbs*); the lifecycle's own rules are tested in job_lifecycle_test.go.
//
//	CreateClassifiesAgainstAHandComputedOracle      24 messy lines against an oracle written by hand; the rows in the table equal the input; the preview's counts equal the oracle's
//	CreateBatchesAndTheRowLimit                     a list longer than one insert batch, duplicates that name a row of an earlier batch; MaxRows is accepted and MaxRows+1 refused
//	CreateRefusals                                  every request the store refuses before it writes, each with its twin; nothing is ever written by a refusal
//	ChainedSendTakesTheSourceVerdicts               rows from a verify job's results by verdict, both choices for unknown, the refusals of a bad source
//	UsersNeverSeeEachOthersJobs                     every method, another user's job and a missing job are the same ErrNotFound; nothing is written
//	PreviewIsASnapshot                              a repeat moves nothing; price, content, list and choice each change the fingerprint; a suppression changes the counts only
//	ConfirmRefusesAStalePreviewAndIgnoresANewSuppression
//	ReachableUnitsDecideTheReserve                  vendor off: 1 per row; on: 2; a preview taken before the flip is stale
//	ConfirmTwiceWithOneKeyIsOneConfirm / TheSameKeyForAnotherJobIsRefused / TwoRequestsWithOneKeyRacing...
//	ShortfallIsExactAndNothingIsWritten             a balance that covers part of the list: the exact shortfall, no ledger row, no reserve, the job still previewed
//	ModuleThree...                                  entitlement, suspension, the pre-check (infeasible, unavailable, scheduled), no lock held while the module is asked
//	ScheduledConfirm                                the start window, the deadline = start + the max delay, by the database's clock
//	ConcurrentConfirms... / ControlWithoutTheSlotIndexBothWin    the forced interleave of two confirms of one (user, module): one winner and no partial reserve; both win without the index
//	CancelBeforeStart... / PauseAndResume... / TheModuleDecidesWhatARunningJobCanDo / DiscardAndReopen / EveryMoveWritesItsEvent / ListPagesByKeyset / ConfirmStatusRefusals

func TestJobsStore_CreateClassifiesAgainstAHandComputedOracle(t *testing.T) {
	e := newJbsEnv(t)
	A := mnyUserA
	fix := jbsOracle()
	in := make([]jobs.RowIn, len(fix))
	for i, r := range fix {
		in[i] = jobs.RowIn{Email: r.in}
	}
	j := e.createRows(A, "m1", in)
	if j.Status != jobs.StatusDraft || j.Module != "m1" || j.UserID != A || j.Content != nil || j.Settings.UnknownChoice != jobs.UnknownExclude || j.SourceJobID != "" || j.Preview != nil {
		t.Fatalf("the new job is %+v, want a draft of module m1 with no content, no source, no preview and the default choice", j)
	}

	// the number of rows in the table equals the input, and every row equals the oracle
	if n := e.int("SELECT count(*) FROM platform.job_rows WHERE job_id = $1", j.ID); int(n) != len(in) {
		t.Fatalf("job_rows holds %d rows, the input had %d", n, len(in))
	}
	got := e.rowsOf(j.ID)
	if p := jbsCompareOracle(fix, got); len(p) != 0 {
		t.Fatalf("the rows in the database differ from the hand-computed oracle:\n  %s", strings.Join(p, "\n  "))
	}
	// THE CONTROL: the comparison can fail. A deliberately wrong oracle (three lines changed) must be reported, once per line.
	wrong := append([]jbsOracleRow(nil), fix...)
	wrong[2].state = jobs.StatePending // a repeat claimed to be a first occurrence
	wrong[4].reason = "no_at"          // an empty line claimed to lack an '@'
	wrong[14].dupOf = 1                // a repeat claimed to repeat the wrong row
	if p := jbsCompareOracle(wrong, got); len(p) != 3 {
		t.Fatalf("the comparison found %d differences in an oracle that is wrong on 3 lines, want 3: %v", len(p), p)
	}

	// a malformed row names its reason as a word, never the address; a row keeps what the user typed wherever the database can store it
	for i, r := range got {
		if r.State == jobs.StateMalformed && (r.Reason == "" || strings.ContainsAny(r.Reason, "@. ") || len(r.Reason) > 32) {
			t.Errorf("row %d: the reason %q of a malformed row is not a word", i+1, r.Reason)
		}
		if s := in[i].Email; utf8.ValidString(s) && !strings.ContainsRune(s, 0) && len(s) <= jobs.MaxRawBytes && r.Raw != s {
			t.Errorf("row %d: email_raw is %q, want what the user typed", i+1, r.Raw)
		}
		if !utf8.ValidString(r.Raw) || len(r.Raw) > jobs.MaxRawBytes {
			t.Errorf("row %d: email_raw is not storable text", i+1)
		}
	}

	// the preview's counts equal the oracle's: 6 pending (1, 2, 13, 14, 16, 24), 5 duplicates (3, 4, 15, 17, 23), 13 malformed
	var pending, dups, bad int
	for _, r := range fix {
		switch r.state {
		case jobs.StatePending:
			pending++
		case jobs.StateDuplicate:
			dups++
		case jobs.StateMalformed:
			bad++
		}
	}
	if pending != 6 || dups != 5 || bad != 13 {
		t.Fatalf("the oracle itself has %d pending, %d duplicate, %d malformed rows, want 6, 5, 13 (hand count)", pending, dups, bad)
	}
	p := e.preview(A, j.ID)
	wantReasons := map[string]int{"empty": 2, "no_at": 1, "multiple_at": 1, "empty_local": 1, "empty_domain": 1, "domain_no_dot": 1, "local_too_long": 1, "bad_idna": 1,
		jobs.ReasonInvalidText: 2, "domain_too_long": 1, jobs.ReasonTooLong: 1}
	if p.Rows != 24 || p.Pending != 6 || p.Malformed != 13 || p.Duplicates != 5 || p.Excluded != 0 || p.Suppressed != 0 || p.MissingFields != 0 || p.Units != 6 {
		t.Errorf("the preview counts rows %d pending %d malformed %d duplicates %d excluded %d suppressed %d missing %d units %d; want 24, 6, 13, 5, 0, 0, 0, 6", p.Rows, p.Pending, p.Malformed, p.Duplicates, p.Excluded, p.Suppressed, p.MissingFields, p.Units)
	}
	if !reflect.DeepEqual(p.MalformedReasons, wantReasons) {
		t.Errorf("the malformed reasons are %v, want %v", p.MalformedReasons, wantReasons)
	}
	if !p.Metered || p.UnitCost != 1 || p.RateUnit != "m1_cache" || p.Credits != 6 || p.Available != 0 || p.Shortfall != 6 || p.UnknownChoice != jobs.UnknownExclude || p.Verdicts != nil {
		t.Errorf("the preview prices %+v, want metered, rate 1 of m1_cache (the first of the equal prices), 6 credits, 0 available, 6 short, no verdicts", p)
	}

	// the stored job carries the same preview and has moved to previewed
	g, err := e.jobs.Get(testCtx(t), A, j.ID)
	mnyNoErr(t, "Get", err)
	if g.Status != jobs.StatusPreviewed || g.PreviewFingerprint != p.Fingerprint || g.Preview == nil || g.Preview.Credits != 6 || g.Preview.Units != 6 || g.PreviewedAt == nil {
		t.Errorf("the previewed job is %+v, want status previewed with the preview that was returned", g)
	}
	// the control of the whole test: a clean list of the same size is all pending and costs its length
	clean := e.createRows(A, "m1", jbsAddrs(24, "k"))
	if cp := e.preview(A, clean.ID); cp.Pending != 24 || cp.Malformed != 0 || cp.Duplicates != 0 || cp.Credits != 24 {
		t.Errorf("a list of 24 clean addresses previews as %+v, want 24 pending and 24 credits", cp)
	}
}

func TestJobsStore_CreateBatchesAndTheRowLimit(t *testing.T) {
	e := newJbsEnv(t, jobs.WithMaxRows(6000))
	A := mnyUserA
	// 5003 rows: the store inserts in batches, so the last three repeat rows of an EARLIER batch (a duplicate names the row it repeats, across the batch boundary)
	in := append(jbsAddrs(5000, "b"), jobs.RowIn{Email: "B00001@EXAMPLE.TEST"}, jobs.RowIn{Email: "b00002@example.test."}, jobs.RowIn{Email: " b00003@example.test "})
	j := e.createRows(A, "m1", in)
	rows := e.rowsOf(j.ID)
	if len(rows) != 5003 {
		t.Fatalf("%d rows stored, want 5003", len(rows))
	}
	for i := 0; i < 3; i++ {
		r := rows[5000+i]
		if r.State != jobs.StateDuplicate || r.DupOf != rows[i].ID || r.Ordinal != 5001+i {
			t.Errorf("row %d: state %s duplicate_of %q ordinal %d, want a duplicate of row %d (%s)", 5001+i, r.State, r.DupOf, r.Ordinal, i+1, rows[i].ID)
		}
	}
	if c := e.stateCounts(j.ID); c[jobs.StatePending] != 5000 || c[jobs.StateDuplicate] != 3 || len(c) != 2 {
		t.Errorf("states %v, want 5000 pending and 3 duplicates", c)
	}

	// the limit: exactly MaxRows is accepted (the twin), one more is refused as ErrTooManyRows naming the limit and writes nothing
	exact := e.createRows(A, "m1", jbsAddrs(6000, "m"))
	if n := e.int("SELECT count(*) FROM platform.job_rows WHERE job_id = $1", exact.ID); n != 6000 {
		t.Fatalf("a list of exactly the limit stored %d rows", n)
	}
	before := e.writes()
	_, err := e.jobs.CreateDraft(testCtx(t), A, jobs.CreateReq{Module: "m1", Rows: jbsAddrs(6001, "n")})
	var tm *jobs.TooManyRowsError
	if !errors.Is(err, jobs.ErrTooManyRows) || !errors.As(err, &tm) || tm.Limit != 6000 {
		t.Fatalf("a list of 6001 rows: got %v, want ErrTooManyRows with limit 6000", err)
	}
	e.requireWrites("a list over the limit", before)
	if jobs.DefaultMaxRows != 100000 {
		t.Errorf("the default row limit is %d, want 100000 (module 1's own limit)", jobs.DefaultMaxRows)
	}
}

func TestJobsStore_CreateRefusals(t *testing.T) {
	e := newJbsEnv(t)
	A := mnyUserA
	ctx := testCtx(t)
	rows := jbsAddrs(2, "x")
	manyFields := map[string]string{}
	for i := 0; i < jobs.MaxFieldKeys+1; i++ {
		manyFields[fmt.Sprintf("k%d", i)] = "v"
	}
	const someJob = "0f0f0f0f-0000-4000-8000-0000000000bb"
	for _, c := range []struct {
		name string
		req  jobs.CreateReq
		want string
	}{
		{"no rows", jobs.CreateReq{Module: "m1"}, "at least one row"},
		{"a module that does not exist", jobs.CreateReq{Module: "m9", Rows: rows}, "module must be m1, m2 or m3"},
		{"a verify job with content", jobs.CreateReq{Module: "m1", Rows: rows, Content: jbsContent()}, "a verify job carries no content"},
		{"a send job with no content", jobs.CreateReq{Module: "m2", Rows: rows}, "content is required for a send job"},
		{"a send job whose content has no body", jobs.CreateReq{Module: "m3", Rows: rows, Content: &jobs.Content{Subject: "s"}}, "content needs a body"},
		{"a subject with a line break", jobs.CreateReq{Module: "m2", Rows: rows, Content: &jobs.Content{Subject: "a\r\nb", Text: "t"}}, "content.subject must be one line"},
		{"merge data over the limit", jobs.CreateReq{Module: "m1", Rows: []jobs.RowIn{{Email: "a@example.test", Fields: manyFields}}}, "merge fields"},
		{"include on a list that is not chained", jobs.CreateReq{Module: "m2", Rows: rows, Content: jbsContent(), Settings: jobs.Settings{UnknownChoice: "include"}}, "unknown_choice"},
		{"a choice outside the vocabulary", jobs.CreateReq{Module: "m2", Rows: rows, Content: jbsContent(), Settings: jobs.Settings{UnknownChoice: "maybe"}}, "unknown_choice"},
		{"a source for a verify job", jobs.CreateReq{Module: "m1", SourceJobID: someJob}, "only a send job"},
		{"rows with a source", jobs.CreateReq{Module: "m2", Rows: rows, Content: jbsContent(), SourceJobID: someJob}, "none may be supplied"},
		{"a source that does not exist", jobs.CreateReq{Module: "m2", Content: jbsContent(), SourceJobID: someJob}, "the source job does not exist"},
		{"a source that is not an id", jobs.CreateReq{Module: "m2", Content: jbsContent(), SourceJobID: "x"}, "the source job does not exist"},
	} {
		before := e.writes()
		_, err := e.jobs.CreateDraft(ctx, A, c.req)
		jbsBadRequest(t, c.name, err, c.want)
		e.requireWrites(c.name, before)
	}
	// the twins: each of these is the nearest request that is accepted
	atLimit := map[string]string{}
	for i := 0; i < jobs.MaxFieldKeys; i++ {
		atLimit[fmt.Sprintf("k%d", i)] = "v"
	}
	for name, req := range map[string]jobs.CreateReq{
		"a verify job":                   {Module: "m1", Rows: rows},
		"a send job with content":        {Module: "m2", Rows: rows, Content: jbsContent()},
		"a module 3 job with text only":  {Module: "m3", Rows: rows, Content: &jobs.Content{Subject: "s", Text: "t"}},
		"merge data at the limit":        {Module: "m1", Rows: []jobs.RowIn{{Email: "a@example.test", Fields: atLimit}}},
		"the default choice written":     {Module: "m2", Rows: rows, Content: jbsContent(), Settings: jobs.Settings{UnknownChoice: "exclude"}},
		"a subject of exactly 998 bytes": {Module: "m2", Rows: rows, Content: &jobs.Content{Subject: strings.Repeat("s", jobs.MaxSubjectBytes), Text: "t"}},
		"a body of exactly 1 MiB (html)": {Module: "m2", Rows: rows, Content: &jobs.Content{Subject: "s", HTML: strings.Repeat("h", jobs.MaxBodyBytes)}},
		"a reply-to that is one address": {Module: "m2", Rows: rows, Content: &jobs.Content{Subject: "s", Text: "t", ReplyTo: "r@example.test"}},
	} {
		if _, err := e.jobs.CreateDraft(ctx, A, req); err != nil {
			t.Errorf("the twin %q was refused: %v", name, err)
		}
	}
	// stored content is what was given (the subject trimmed), under frozen_settings.content, and a verify job has none
	g, err := e.jobs.CreateDraft(ctx, A, jobs.CreateReq{Module: "m2", Rows: rows, Content: &jobs.Content{Subject: "  Hi  ", HTML: "<b>x</b>", Text: "x", ReplyTo: " r@example.test "}})
	mnyNoErr(t, "CreateDraft", err)
	if g.Content == nil || *g.Content != (jobs.Content{Subject: "Hi", HTML: "<b>x</b>", Text: "x", ReplyTo: "r@example.test"}) {
		t.Errorf("the stored content is %+v", g.Content)
	}
	if e.str("SELECT frozen_settings #>> '{content,subject}' FROM platform.jobs WHERE id = $1", g.ID) != "Hi" {
		t.Error("the content is not under frozen_settings.content")
	}
	// a module the platform switched off is refused, and accepted again when switched back on
	e.exec("UPDATE platform.modules SET active = false WHERE id = 'm3'")
	before := e.writes()
	_, err = e.jobs.CreateDraft(ctx, A, jobs.CreateReq{Module: "m3", Rows: rows, Content: jbsContent()})
	jbsBadRequest(t, "a module that is not active", err, "is not active")
	e.requireWrites("a module that is not active", before)
	e.exec("UPDATE platform.modules SET active = true WHERE id = 'm3'")
	if _, err := e.jobs.CreateDraft(ctx, A, jobs.CreateReq{Module: "m3", Rows: rows, Content: jbsContent()}); err != nil {
		t.Errorf("the control: module 3 switched back on was refused: %v", err)
	}
}

func TestJobsStore_ChainedSendTakesTheSourceVerdicts(t *testing.T) {
	e := newJbsEnv(t)
	A, B := mnyUserA, mnyUserB
	e.fund(A, "m1", 100)
	e.fund(A, "m2", 100)

	// a verify job that succeeded: six distinct addresses (the module's verdicts below), one malformed line and one repeat (the platform's own results)
	in := []jobs.RowIn{{Email: "v1@example.test"}, {Email: "v2@example.test"}, {Email: "v3@example.test"}, {Email: "v4@example.test"}, {Email: "v5@example.test"}, {Email: "v6@example.test"}, {Email: "not an address"}, {Email: "V1@example.test"}}
	src := e.createRows(A, "m1", in)
	e.confirm(A, src.ID, e.preview(A, src.ID).Fingerprint)
	verdicts := map[int]string{1: "valid", 2: "valid", 3: "risky", 4: "unknown", 5: "invalid", 6: "unknown"}
	for ord, v := range verdicts {
		e.exec(`INSERT INTO platform.job_results (job_id, user_id, item_ref, ordinal, email, origin, module_state, decision, rule)
			SELECT r.job_id, r.user_id, r.id, r.ordinal, r.email_norm, 'module', $3, 'free', 'M1-19' FROM platform.job_rows r WHERE r.job_id = $1 AND r.ordinal = $2`, src.ID, ord, v)
	}
	jobWalkTo(t, e.admin, src.ID, "succeeded")
	e.requireBooks("a succeeded verify job")
	srcRows := e.rowsOf(src.ID)

	chain := func(user string, choice string) (jobs.Job, error) {
		return e.jobs.CreateDraft(testCtx(t), user, jobs.CreateReq{Module: "m2", Content: jbsContent(), SourceJobID: src.ID, Settings: jobs.Settings{UnknownChoice: choice}})
	}
	// the default: valid and risky are sent to, unknown and invalid are not
	ex, err := chain(A, "")
	mnyNoErr(t, "chain (default choice)", err)
	if ex.SourceJobID != src.ID || ex.Settings.UnknownChoice != jobs.UnknownExclude || ex.Module != "m2" {
		t.Fatalf("the chained job is %+v", ex)
	}
	got := e.rowsOf(ex.ID)
	wantState := []string{jobs.StatePending, jobs.StatePending, jobs.StatePending, jobs.StateExcluded, jobs.StateExcluded, jobs.StateExcluded}
	wantReason := []string{"", "", "", "unknown", "invalid", "unknown"}
	if len(got) != 6 {
		t.Fatalf("the chained job has %d rows, want one per module result (6): the platform's own results of the source are not chained", len(got))
	}
	for i, r := range got {
		if r.Ordinal != i+1 || r.State != wantState[i] || r.Reason != wantReason[i] || r.Norm != fmt.Sprintf("v%d@example.test", i+1) {
			t.Errorf("chained row %d: %+v, want ordinal %d, %s, reason %q, v%d@example.test", i+1, r, i+1, wantState[i], wantReason[i], i+1)
		}
		if src := e.str("SELECT COALESCE(source_item::text, '') FROM platform.job_rows WHERE id = $1", r.ID); src != srcRows[i].ID {
			t.Errorf("chained row %d: source_item %s, want the source row %s", i+1, src, srcRows[i].ID)
		}
	}
	p := e.preview(A, ex.ID)
	if p.Rows != 6 || p.Pending != 3 || p.Excluded != 3 || p.Units != 3 || p.Credits != 3 || p.Selected != 3 || p.UnknownChoice != jobs.UnknownExclude ||
		!reflect.DeepEqual(p.Verdicts, map[string]int{"valid": 2, "invalid": 1, "risky": 1, "unknown": 2}) {
		t.Errorf("the chained preview is %+v, want 3 pending, 3 excluded, 3 credits, verdicts valid 2 invalid 1 risky 1 unknown 2, 3 selected", p)
	}
	// THE CONTROL: the choice for unknown flips exactly the unknown rows
	in2, err := chain(A, jobs.UnknownInclude)
	mnyNoErr(t, "chain (include)", err)
	p2 := e.preview(A, in2.ID)
	if p2.Pending != 5 || p2.Excluded != 1 || p2.Selected != 5 || p2.Credits != 5 || p2.UnknownChoice != jobs.UnknownInclude {
		t.Errorf("with the unknown rows included the preview is %+v, want 5 pending, 1 excluded, 5 selected, 5 credits", p2)
	}
	if p.Fingerprint == p2.Fingerprint {
		t.Error("the two choices share a fingerprint")
	}
	// confirming holds credits for the selected rows only and records the excluded rows as free platform results
	r := e.confirm(A, ex.ID, p.Fingerprint)
	if r.Reserved != 3 || r.Pending != 3 || e.ruleCounts(ex.ID)[jobs.RuleExcluded] != 3 || len(e.ruleCounts(ex.ID)) != 1 {
		t.Errorf("confirm reserved %d for %d rows with results %v, want 3 credits and three %s results", r.Reserved, r.Pending, e.ruleCounts(ex.ID), jobs.RuleExcluded)
	}
	e.requireBooks("a confirmed chained send")

	// the refusals of a bad source (and each is the same answer whether the job is another user's or does not exist)
	_, errOther := chain(B, "")
	_, errMissing := e.jobs.CreateDraft(testCtx(t), B, jobs.CreateReq{Module: "m2", Content: jbsContent(), SourceJobID: "0f0f0f0f-0000-4000-8000-0000000000cc"})
	jbsBadRequest(t, "a source of another user", errOther, "the source job does not exist")
	jbsBadRequest(t, "a source that does not exist", errMissing, "the source job does not exist")
	if errOther.Error() != errMissing.Error() {
		t.Errorf("another user's source and a missing one are told apart: %q vs %q", errOther, errMissing)
	}
	draft := e.create(A, "m1", 2)
	_, err = e.jobs.CreateDraft(testCtx(t), A, jobs.CreateReq{Module: "m2", Content: jbsContent(), SourceJobID: draft.ID})
	jbsBadRequest(t, "a source that has not succeeded", err, "has not succeeded")
	_, err = e.jobs.CreateDraft(testCtx(t), A, jobs.CreateReq{Module: "m2", Content: jbsContent(), SourceJobID: ex.ID})
	jbsBadRequest(t, "a source that is not a verify job", err, "not a verify job")
	// a succeeded verify job that holds no result of the module has nothing to chain
	noRes := e.createRows(A, "m1", jbsAddrs(2, "z"))
	e.confirm(A, noRes.ID, e.preview(A, noRes.ID).Fingerprint)
	jobWalkTo(t, e.admin, noRes.ID, "succeeded")
	_, err = e.jobs.CreateDraft(testCtx(t), A, jobs.CreateReq{Module: "m2", Content: jbsContent(), SourceJobID: noRes.ID})
	jbsBadRequest(t, "a source with nothing to chain", err, "no results from the module")
}

func TestJobsStore_UsersNeverSeeEachOthersJobs(t *testing.T) {
	e := newJbsEnv(t)
	A, B := mnyUserA, mnyUserB
	e.fund(A, "m1", 100)
	ja, pa := e.previewed(A, "m1", 3)
	jc, _ := e.confirmed(A, "m2", 2)
	const missing = "0f0f0f0f-0000-4000-8000-0000000000aa"
	key := e.key()
	ops := map[string]func(user, job string) error{
		"Get":               func(u, j string) error { _, err := e.jobs.Get(testCtx(t), u, j); return err },
		"Events":            func(u, j string) error { _, err := e.jobs.Events(testCtx(t), u, j, 0, 0); return err },
		"Preview":           func(u, j string) error { _, err := e.jobs.Preview(testCtx(t), u, j); return err },
		"Confirm":           func(u, j string) error { _, err := e.tryConfirm(u, j, pa.Fingerprint, key, nil); return err },
		"DiscardDraft":      func(u, j string) error { _, err := e.jobs.DiscardDraft(testCtx(t), u, j); return err },
		"Reopen":            func(u, j string) error { _, err := e.jobs.Reopen(testCtx(t), u, j); return err },
		"CancelBeforeStart": func(u, j string) error { _, err := e.jobs.CancelBeforeStart(testCtx(t), u, j); return err },
		"PauseScheduled":    func(u, j string) error { _, err := e.jobs.PauseScheduled(testCtx(t), u, j); return err },
		"ResumeScheduled":   func(u, j string) error { _, err := e.jobs.ResumeScheduled(testCtx(t), u, j); return err },
	}
	before := e.writes()
	for name, op := range ops {
		for _, target := range []string{ja.ID, jc.ID} {
			errOther, errMissing := op(B, target), op(B, missing)
			jbsIs(t, name+" on another user's job", errOther, jobs.ErrNotFound)
			jbsIs(t, name+" on a job that does not exist", errMissing, jobs.ErrNotFound)
			if errOther.Error() != errMissing.Error() {
				t.Errorf("%s: another user's job and a missing job are told apart: %q vs %q", name, errOther, errMissing)
			}
		}
	}
	e.requireWrites("another user poking at every method", before)
	// the controls: the owner reads and previews the very same jobs, and the lists are each user's own
	if g, err := e.jobs.Get(testCtx(t), A, ja.ID); err != nil || g.ID != ja.ID {
		t.Fatalf("the owner cannot read their own job: %v", err)
	}
	if evs, err := e.jobs.Events(testCtx(t), A, ja.ID, 0, 0); err != nil || len(evs) == 0 {
		t.Fatalf("the owner cannot read their job's events: %v (%d)", err, len(evs))
	}
	e.preview(A, ja.ID)
	jb := e.create(B, "m1", 1)
	la, _, err := e.jobs.List(testCtx(t), A, jobs.ListOpts{})
	mnyNoErr(t, "List(A)", err)
	lb, _, err := e.jobs.List(testCtx(t), B, jobs.ListOpts{})
	mnyNoErr(t, "List(B)", err)
	if len(la) != 2 || len(lb) != 1 || lb[0].ID != jb.ID {
		t.Errorf("A lists %d jobs and B lists %v, want 2 and just B's own job", len(la), lb)
	}
	for _, j := range la {
		if j.UserID != A {
			t.Errorf("A's list holds a job of user %s", j.UserID)
		}
	}
}

func TestJobsStore_PreviewIsASnapshot(t *testing.T) {
	fixed := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	e := newJbsEnv(t, jobs.WithClock(func() time.Time { return fixed }))
	A := mnyUserA
	e.fund(A, "m2", 1000)
	j := e.create(A, "m2", 10)
	p1 := e.preview(A, j.ID)
	if !p1.ComputedAt.Equal(fixed) || len(p1.Fingerprint) != 64 {
		t.Errorf("computed_at %v fingerprint %q, want the store's clock and 64 hex digits", p1.ComputedAt, p1.Fingerprint)
	}
	wantEvents := []string{"created|>|user|" + A + "|draft created", "transition|draft>previewed|user|" + A + "|preview requested by the user"}
	if got := e.eventLines(j.ID); !reflect.DeepEqual(got, wantEvents) {
		t.Fatalf("events after the first preview: %v, want %v", got, wantEvents)
	}
	// a repeat recomputes and moves nothing: no transition, no event; and nothing that the preview depends on changed, so the fingerprint is the same
	first := e.str("SELECT previewed_at::text FROM platform.jobs WHERE id = $1", j.ID)
	p2 := e.preview(A, j.ID)
	if p2.Fingerprint != p1.Fingerprint {
		t.Fatalf("a repeat of an unchanged preview has another fingerprint (%s vs %s): the control of every change below", p2.Fingerprint, p1.Fingerprint)
	}
	if got := e.eventLines(j.ID); len(got) != 2 {
		t.Errorf("a repeat of the preview wrote events: %v", got)
	}
	if e.str("SELECT previewed_at::text FROM platform.jobs WHERE id = $1", j.ID) == first {
		t.Error("a repeat of the preview did not store its own time")
	}

	setCost := func(cost int) {
		e.exec("UPDATE platform.unit_costs SET cost = $1, changed_by = $2, change_reason = 'snapshot test' WHERE unit = 'm2_accepted'", cost, mnyAdminA)
	}
	// 1. a price change: the fingerprint changes and so do the credits (the reserve is at the dearest price)
	setCost(2)
	p3 := e.preview(A, j.ID)
	if p3.Fingerprint == p2.Fingerprint || p3.UnitCost != 2 || p3.Credits != 20 || p3.RateUnit != "m2_accepted" {
		t.Errorf("after a price change the preview is %+v, want another fingerprint, rate 2 of m2_accepted, 20 credits", p3)
	}
	setCost(1)
	if p4 := e.preview(A, j.ID); p4.Fingerprint != p1.Fingerprint || p4.Credits != 10 {
		t.Errorf("with the price back the preview is %+v, want the first fingerprint again (a fingerprint is a function of its inputs, not a counter) and 10 credits", p4)
	}
	// 2. a new suppression of a row's address changes the COUNTS and never the fingerprint; the row stays pending (preview only counts)
	e.suppressFor(A, "r00003@example.test")
	p5 := e.preview(A, j.ID)
	if p5.Fingerprint != p1.Fingerprint || p5.Suppressed != 1 || p5.Units != 9 || p5.Credits != 9 || p5.Pending != 10 {
		t.Errorf("after a suppression the preview is %+v, want the same fingerprint, 1 suppressed, 9 units, 9 credits, 10 pending", p5)
	}
	if c := e.stateCounts(j.ID); c[jobs.StatePending] != 10 || c[jobs.StateSuppressed] != 0 {
		t.Errorf("a preview wrote the suppression into the rows: %v", c)
	}
	// 3. different content (frozen_settings changes only while the job is draft or previewed)
	setSubject := func(s string) {
		e.exec(`UPDATE platform.jobs SET frozen_settings = jsonb_set(frozen_settings, '{content,subject}', to_jsonb($2::text)) WHERE id = $1`, j.ID, s)
	}
	setSubject("another subject")
	if p6 := e.preview(A, j.ID); p6.Fingerprint == p1.Fingerprint {
		t.Error("another content left the fingerprint unchanged")
	}
	setSubject(jbsContent().Subject)
	if p7 := e.preview(A, j.ID); p7.Fingerprint != p1.Fingerprint {
		t.Error("the content restored, the fingerprint did not come back")
	}
	// 4. a different row list
	e.exec("UPDATE platform.job_rows SET email_norm = 'changed@example.test' WHERE job_id = $1 AND ordinal = 4", j.ID)
	if p8 := e.preview(A, j.ID); p8.Fingerprint == p1.Fingerprint {
		t.Error("another row list left the fingerprint unchanged")
	}
	e.exec("UPDATE platform.job_rows SET email_norm = 'r00004@example.test' WHERE job_id = $1 AND ordinal = 4", j.ID)
	if p9 := e.preview(A, j.ID); p9.Fingerprint != p1.Fingerprint {
		t.Error("the row list restored, the fingerprint did not come back")
	}
	// 5. the unknown choice
	e.exec(`UPDATE platform.jobs SET frozen_settings = jsonb_set(frozen_settings, '{settings,unknown_choice}', '"include"') WHERE id = $1`, j.ID)
	if p10 := e.preview(A, j.ID); p10.Fingerprint == p1.Fingerprint {
		t.Error("another choice for unknown verdicts left the fingerprint unchanged")
	}
	// a preview is of THIS job: an identical list on another job has another fingerprint
	j2, q := e.previewed(A, "m2", 10)
	if q.Fingerprint == p1.Fingerprint || j2.ID == j.ID {
		t.Error("two jobs with one list share a fingerprint")
	}
	// only a draft or a previewed job has a preview
	e.confirm(A, j2.ID, q.Fingerprint)
	_, err := e.jobs.Preview(testCtx(t), A, j2.ID)
	jbsNotAllowed(t, "a preview of a confirmed job", err, "job_status_has_no_preview")
}

func TestJobsStore_ConfirmRefusesAStalePreviewAndIgnoresANewSuppression(t *testing.T) {
	e := newJbsEnv(t)
	A, B := mnyUserA, mnyUserB
	e.fund(A, "m2", 1000)
	e.fund(B, "m2", 1000)
	j, p := e.previewed(A, "m2", 10)

	// a fingerprint that is not the job's is stale; one that is not a fingerprint is a bad request
	before := e.writes()
	_, err := e.tryConfirm(A, j.ID, strings.Repeat("0", 64), e.key(), nil)
	jbsIs(t, "a well-formed fingerprint of nothing", err, jobs.ErrStalePreview)
	e.requireWrites("a wrong fingerprint", before)
	for _, bad := range []string{"", p.Fingerprint[:63], strings.ToUpper(p.Fingerprint), p.Fingerprint + "0"} {
		_, err := e.tryConfirm(A, j.ID, bad, e.key(), nil)
		jbsBadRequest(t, fmt.Sprintf("the fingerprint %q", bad), err, "fingerprint")
	}
	e.requireWrites("malformed fingerprints", before)

	// a price change after the preview: ErrStalePreview, and NOTHING is written (no reserve, no snapshot, the job still previewed)
	e.exec("UPDATE platform.unit_costs SET cost = 2, changed_by = $1, change_reason = 'stale test' WHERE unit = 'm2_accepted'", mnyAdminA)
	before = e.writes()
	_, err = e.tryConfirm(A, j.ID, p.Fingerprint, e.key(), nil)
	jbsIs(t, "confirm after a price change", err, jobs.ErrStalePreview)
	e.requireWrites("a stale confirm", before)
	if s := e.jobStatus(j.ID); s != "previewed" || e.reserveRows(j.ID) != 0 || len(e.frozen(j.ID)) != 0 {
		t.Errorf("after the refusal the job is %s with %d reserve rows and %v frozen", s, e.reserveRows(j.ID), e.frozen(j.ID))
	}
	// THE CONTROL: preview again and confirm what the new preview says: the reserve is at the NEW dearest price
	p2 := e.preview(A, j.ID)
	r := e.confirm(A, j.ID, p2.Fingerprint)
	if r.Reserved != 20 || e.frozen(j.ID)["m2_accepted"] != 2 || e.int("SELECT unit_cost FROM platform.ledger WHERE job_id = $1 AND kind = 'reserve'", j.ID) != 2 {
		t.Errorf("the confirm reserved %d with prices %v, want 20 at rate 2", r.Reserved, e.frozen(j.ID))
	}

	// a suppression added after the preview does NOT make it stale: the confirm applies it, holds credits for the rest and never for more than the preview said
	j2, q := e.previewed(B, "m2", 10)
	e.suppressFor(B, "r00002@example.test")
	r2 := e.confirm(B, j2.ID, q.Fingerprint)
	if r2.Suppressed != 1 || r2.Pending != 9 || r2.Reserved != 18 || r2.Reserved > q.Credits {
		t.Errorf("confirm after a new suppression: suppressed %d pending %d reserved %d (previewed %d credits), want 1, 9, 18 and never more than previewed", r2.Suppressed, r2.Pending, r2.Reserved, q.Credits)
	}
	rows := e.rowsOf(j2.ID)
	if rows[1].State != jobs.StateSuppressed || e.stateCounts(j2.ID)[jobs.StatePending] != 9 {
		t.Errorf("row 2 is %s and %d rows are pending, want the suppressed row and 9 pending", rows[1].State, e.stateCounts(j2.ID)[jobs.StatePending])
	}
	if rc := e.ruleCounts(j2.ID); rc[jobs.RuleSuppressed] != 1 || len(rc) != 1 {
		t.Errorf("results %v, want exactly one %s", rc, jobs.RuleSuppressed)
	}
	if e.int("SELECT units FROM platform.ledger WHERE job_id = $1 AND kind = 'reserve'", j2.ID) != 9 || e.reserveRows(j2.ID) != 1 {
		t.Error("the reserve row does not hold the 9 units that reach the module")
	}
	e.requireBooks("after the confirms")
}

func TestJobsStore_ReachableUnitsDecideTheReserve(t *testing.T) {
	e := newJbsEnv(t)
	A, B, C := mnyUserA, mnyUserB, mnyUserC
	for _, u := range []string{A, B, C} {
		e.fund(u, "m1", 100)
	}
	setVendor := func(on bool) { e.exec("SELECT platform.set_unit_reachable('m1_vendor', $1, 'jobs store test')", on) }

	// the vendor fallback ships off: a verify job reserves at 1 per row, and its snapshot does not hold the vendor unit
	jA, pA := e.previewed(A, "m1", 5)
	if pA.UnitCost != 1 || pA.Credits != 5 || pA.RateUnit != "m1_cache" {
		t.Errorf("vendor off: the preview is rate %d of %s, %d credits; want rate 1 of m1_cache, 5 credits", pA.UnitCost, pA.RateUnit, pA.Credits)
	}
	rA := e.confirm(A, jA.ID, pA.Fingerprint)
	if rA.Reserved != 5 || e.int("SELECT unit_cost FROM platform.ledger WHERE job_id = $1 AND kind = 'reserve'", jA.ID) != 1 {
		t.Errorf("vendor off: reserved %d, want 5 at 1 per row", rA.Reserved)
	}
	mnyPricesEqual(t, "vendor off: the price snapshot", e.frozen(jA.ID), map[string]int{"m1_own": 1, "m1_cache": 1})

	// a preview taken BEFORE the flip is stale after it (the set of reachable units is part of the fingerprint); a fresh one reserves at the vendor's 2
	jB, pB := e.previewed(B, "m1", 5)
	setVendor(true)
	before := e.writes()
	_, err := e.tryConfirm(B, jB.ID, pB.Fingerprint, e.key(), nil)
	jbsIs(t, "a confirm of a preview taken with the vendor off, after it was switched on", err, jobs.ErrStalePreview)
	e.requireWrites("the stale confirm", before)
	pB2 := e.preview(B, jB.ID)
	if pB2.UnitCost != 2 || pB2.RateUnit != "m1_vendor" || pB2.Credits != 10 {
		t.Errorf("vendor on: the preview is rate %d of %s, %d credits; want rate 2 of m1_vendor, 10 credits", pB2.UnitCost, pB2.RateUnit, pB2.Credits)
	}
	rB := e.confirm(B, jB.ID, pB2.Fingerprint)
	if rB.Reserved != 10 || e.int("SELECT unit_cost FROM platform.ledger WHERE job_id = $1 AND kind = 'reserve'", jB.ID) != 2 {
		t.Errorf("vendor on: reserved %d, want 10 at 2 per row", rB.Reserved)
	}
	mnyPricesEqual(t, "vendor on: the price snapshot", e.frozen(jB.ID), map[string]int{"m1_own": 1, "m1_cache": 1, "m1_vendor": 2})
	// the job confirmed before the flip keeps its own snapshot and reserve
	mnyPricesEqual(t, "the earlier job's snapshot", e.frozen(jA.ID), map[string]int{"m1_own": 1, "m1_cache": 1})

	// and back off: the control of the symmetry
	setVendor(false)
	jC, pC := e.previewed(C, "m1", 5)
	if rC := e.confirm(C, jC.ID, pC.Fingerprint); rC.Reserved != 5 || pC.UnitCost != 1 {
		t.Errorf("vendor off again: reserved %d at rate %d, want 5 at 1", rC.Reserved, pC.UnitCost)
	}
	e.requireBooks("after the three confirms")
}

func TestJobsStore_ConfirmTwiceWithOneKeyIsOneConfirm(t *testing.T) {
	e := newJbsEnv(t)
	A := mnyUserA
	e.fund(A, "m1", 100)
	j, p := e.previewed(A, "m1", 4)
	key := e.key()
	r1, err := e.tryConfirm(A, j.ID, p.Fingerprint, key, nil)
	mnyNoErr(t, "the first confirm", err)
	r2, err := e.tryConfirm(A, j.ID, p.Fingerprint, key, nil)
	mnyNoErr(t, "the second confirm with the same key", err)
	if r1.Replayed || !r2.Replayed || r2.Job.ID != j.ID || r2.Job.Status != jobs.StatusConfirmed || r2.Reserved != r1.Reserved || r2.Pending != r1.Pending || r1.Reserved != 4 {
		t.Fatalf("first %+v, second %+v; want the second to be the first again (Replayed, same numbers, status confirmed)", r1, r2)
	}
	if e.reserveRows(j.ID) != 1 || e.int("SELECT count(*) FROM platform.ledger WHERE job_id = $1", j.ID) != 1 {
		t.Errorf("the ledger holds %d reserve rows (%d rows for the job), want exactly one", e.reserveRows(j.ID), e.int("SELECT count(*) FROM platform.ledger WHERE job_id = $1", j.ID))
	}
	e.requireBal("one confirm, however often asked", A, "m1", 96, 4)
	confirms := 0
	for _, l := range e.eventLines(j.ID) {
		if strings.Contains(l, "previewed>confirmed") {
			confirms++
		}
	}
	if confirms != 1 {
		t.Errorf("%d confirm events, want 1", confirms)
	}
	// the replay answers with the job as it is NOW, and a replay with another fingerprint is still the same confirm (the key names it)
	jobWalkTo(t, e.admin, j.ID, "running")
	r3, err := e.tryConfirm(A, j.ID, strings.Repeat("0", 64), key, nil)
	mnyNoErr(t, "the third confirm", err)
	if !r3.Replayed || r3.Job.Status != jobs.StatusRunning || e.reserveRows(j.ID) != 1 {
		t.Errorf("the replay of a job that has started: %+v", r3)
	}
	// the control: a DIFFERENT key for the same job is not a replay (the job is no longer previewed)
	_, err = e.tryConfirm(A, j.ID, p.Fingerprint, e.key(), nil)
	jbsIs(t, "another key for a job that was confirmed", err, jobs.ErrNotPreviewed)
	e.requireBooks("after the replays")
}

func TestJobsStore_TheSameKeyForAnotherJobIsRefused(t *testing.T) {
	e := newJbsEnv(t)
	A, B := mnyUserA, mnyUserB
	e.fund(A, "m1", 100)
	e.fund(A, "m2", 100)
	e.fund(B, "m1", 100)
	j1, p1 := e.previewed(A, "m1", 3)
	j2, p2 := e.previewed(A, "m2", 3)
	key := e.key()
	_, err := e.tryConfirm(A, j1.ID, p1.Fingerprint, key, nil)
	mnyNoErr(t, "the first job's confirm", err)

	before := e.writes()
	_, err = e.tryConfirm(A, j2.ID, p2.Fingerprint, key, nil)
	jbsIs(t, "the key of another job", err, jobs.ErrConfirmKeyReused)
	e.requireWrites("a key that belongs to another job", before)
	if e.jobStatus(j2.ID) != "previewed" || e.reserveRows(j2.ID) != 0 {
		t.Error("the refused job moved or reserved")
	}
	// the controls: the key is the USER's (another user may use it), and a fresh key confirms the very job that was refused
	jB, pB := e.previewed(B, "m1", 3)
	if _, err := e.tryConfirm(B, jB.ID, pB.Fingerprint, key, nil); err != nil {
		t.Errorf("another user could not use the same key: %v", err)
	}
	if _, err := e.tryConfirm(A, j2.ID, p2.Fingerprint, e.key(), nil); err != nil {
		t.Errorf("a fresh key did not confirm the job the first one was refused for: %v", err)
	}

	// the shape of a key: 1 to 128 characters of letters, digits and . _ : -
	const missing = "0f0f0f0f-0000-4000-8000-0000000000dd"
	for _, bad := range []string{"", "has space", "slash/slash", "unicodé", strings.Repeat("k", 129), "new\nline"} {
		_, err := e.tryConfirm(A, missing, p1.Fingerprint, bad, nil)
		jbsBadRequest(t, fmt.Sprintf("the key %q", bad), err, "confirm key")
	}
	for _, ok := range []string{"a", "A.b_c:d-1", strings.Repeat("k", 128)} {
		_, err := e.tryConfirm(A, missing, p1.Fingerprint, ok, nil) // past the key's shape, the job is what is missing
		jbsIs(t, fmt.Sprintf("the key %q", ok), err, jobs.ErrNotFound)
	}
	e.requireBooks("after the key tests")
}

// Two identical requests race (a client that retries too fast): both pass the cheap replay check, both queue on the advisory lock, and the second finds the job confirmed with its own key INSIDE the
// transaction. One confirm, one replay, one reserve.
func TestJobsStore_TwoRequestsWithOneKeyRacingAreOneConfirm(t *testing.T) {
	e := newJbsEnv(t)
	A := mnyUserA
	e.fund(A, "m2", 100)
	j, p := e.previewed(A, "m2", 5)
	key := e.key()
	out := e.interleaveConfirms(A, "m2", [2]jbsCall{{j.ID, p.Fingerprint, key}, {j.ID, p.Fingerprint, key}})
	for i, o := range out {
		if o.Err != nil {
			t.Fatalf("request %d failed: %v", i+1, o.Err)
		}
	}
	if out[0].Res.Replayed == out[1].Res.Replayed {
		t.Errorf("replayed = %v and %v, want exactly one of the two to be the replay", out[0].Res.Replayed, out[1].Res.Replayed)
	}
	if out[0].Res.Reserved != 5 || out[1].Res.Reserved != 5 || out[0].Res.Job.Status != jobs.StatusConfirmed || out[1].Res.Job.Status != jobs.StatusConfirmed {
		t.Errorf("the two answers are %+v and %+v, want the same confirmed job with 5 credits held", out[0].Res, out[1].Res)
	}
	if e.reserveRows(j.ID) != 1 {
		t.Errorf("%d reserve rows, want one", e.reserveRows(j.ID))
	}
	e.requireBal("one reserve for two requests", A, "m2", 95, 5)
	e.requireBooks("after the race")
}

func TestJobsStore_ShortfallIsExactAndNothingIsWritten(t *testing.T) {
	e := newJbsEnv(t)
	A := mnyUserA
	e.fund(A, "m1", 7)
	j, p := e.previewed(A, "m1", 10)
	if p.Credits != 10 || p.Available != 7 || p.Shortfall != 3 {
		t.Fatalf("the preview says %d credits, %d available, %d short; want 10, 7, 3", p.Credits, p.Available, p.Shortfall)
	}
	// a suppression after the preview: the confirm will hold credits for 9 rows, so the shortfall is 2 (and the suppression the transaction wrote must roll back with the refusal)
	e.suppressFor(A, "r00001@example.test")
	before := e.writes()
	_, err := e.tryConfirm(A, j.ID, p.Fingerprint, e.key(), nil)
	var ic jobs.ErrInsufficientCredits
	if !errors.As(err, &ic) || ic.Available != 7 || ic.Needed != 9 || ic.Shortfall != 2 || ic.Raced {
		t.Fatalf("a balance that covers part of the list: got %v, want ErrInsufficientCredits{available 7, needed 9, shortfall 2} decided by the store's own read", err)
	}
	var mic money.ErrInsufficientCredits
	if !errors.As(err, &mic) || mic.Shortfall != 2 {
		t.Errorf("the error is not money's ErrInsufficientCredits too: %v", err)
	}
	e.requireWrites("a confirm the balance does not cover", before)
	if e.jobStatus(j.ID) != "previewed" || e.reserveRows(j.ID) != 0 || len(e.frozen(j.ID)) != 0 || e.stateCounts(j.ID)[jobs.StateSuppressed] != 0 || len(e.ruleCounts(j.ID)) != 0 {
		t.Errorf("the refusal left the job %s, %d reserve rows, snapshot %v, rows %v, results %v", e.jobStatus(j.ID), e.reserveRows(j.ID), e.frozen(j.ID), e.stateCounts(j.ID), e.ruleCounts(j.ID))
	}
	e.requireBal("the balance is untouched", A, "m1", 7, 0)
	// THE CONTROL: two more credits cover exactly the 9 rows that reach the module, and the confirm goes through
	e.fund(A, "m1", 2)
	r, err := e.tryConfirm(A, j.ID, p.Fingerprint, e.key(), nil)
	mnyNoErr(t, "the confirm that fits", err)
	if r.Reserved != 9 || r.Suppressed != 1 || r.Pending != 9 {
		t.Errorf("confirm: reserved %d suppressed %d pending %d, want 9, 1, 9", r.Reserved, r.Suppressed, r.Pending)
	}
	e.requireBal("the whole balance reserved", A, "m1", 0, 9)
	e.requireBooks("after the shortfall test")
}

func TestJobsStore_ConfirmStatusRefusalsAndNothingToDo(t *testing.T) {
	e := newJbsEnv(t)
	A := mnyUserA
	e.fund(A, "m1", 100)
	fp := strings.Repeat("a", 64)

	// a job that is not previewed cannot be confirmed
	draft := e.create(A, "m1", 2)
	before := e.writes()
	_, err := e.tryConfirm(A, draft.ID, fp, e.key(), nil)
	jbsIs(t, "confirm of a draft", err, jobs.ErrNotPreviewed)
	e.requireWrites("confirm of a draft", before)
	discarded := e.create(A, "m1", 2)
	_, err = e.jobs.DiscardDraft(testCtx(t), A, discarded.ID)
	mnyNoErr(t, "DiscardDraft", err)
	_, err = e.tryConfirm(A, discarded.ID, fp, e.key(), nil)
	jbsIs(t, "confirm of a cancelled job", err, jobs.ErrNotPreviewed)

	// a list in which nothing would reach the module: all malformed, or all suppressed after the preview
	bad := e.createRows(A, "m1", []jobs.RowIn{{Email: ""}, {Email: "no at"}, {Email: "a@"}})
	pb := e.preview(A, bad.ID)
	if pb.Pending != 0 || pb.Units != 0 || pb.Credits != 0 || pb.Malformed != 3 || pb.Shortfall != 0 {
		t.Errorf("a list of three malformed lines previews as %+v, want zero units and zero credits (the numbers are the answer)", pb)
	}
	before = e.writes()
	_, err = e.tryConfirm(A, bad.ID, pb.Fingerprint, e.key(), nil)
	jbsIs(t, "confirm of a list with nothing to send", err, jobs.ErrNothingToDo)
	e.requireWrites("confirm of a list with nothing to send", before)

	two, p2 := e.previewed(A, "m1", 2)
	e.suppressFor(A, "r00001@example.test", "r00002@example.test")
	before = e.writes()
	_, err = e.tryConfirm(A, two.ID, p2.Fingerprint, e.key(), nil)
	jbsIs(t, "confirm of a list that suppression empties", err, jobs.ErrNothingToDo)
	e.requireWrites("confirm of a list that suppression empties (the suppression of the rows rolled back)", before)
	if c := e.stateCounts(two.ID); c[jobs.StatePending] != 2 {
		t.Errorf("the refused confirm left the rows %v, want both still pending", c)
	}
	// the control: a list with ONE sendable row confirms, and the platform's results of the other lines are written
	mixed := e.createRows(A, "m1", []jobs.RowIn{{Email: ""}, {Email: "ok@example.test"}, {Email: "a@"}})
	r := e.confirm(A, mixed.ID, e.preview(A, mixed.ID).Fingerprint)
	if r.Pending != 1 || r.Reserved != 1 || e.ruleCounts(mixed.ID)["P-08"] != 2 || len(e.ruleCounts(mixed.ID)) != 1 {
		t.Errorf("a list with one sendable row: %+v with results %v, want 1 pending, 1 credit and two P-08 results", r, e.ruleCounts(mixed.ID))
	}
	e.requireBooks("after the status tests")
}

func TestJobsStore_ModuleThreeNeedsAnEntitlementAndAFreeUser(t *testing.T) {
	e := newJbsEnv(t)
	confirmM3 := func(user string) (jobs.ConfirmResult, string, error) {
		j, p := e.previewed(user, "m3", 4)
		before := e.writes()
		r, err := e.tryConfirm(user, j.ID, p.Fingerprint, e.key(), nil)
		if err != nil {
			e.requireWrites("a refused module 3 confirm", before)
			if e.jobStatus(j.ID) != "previewed" {
				t.Errorf("a refused module 3 confirm moved the job to %s", e.jobStatus(j.ID))
			}
		}
		return r, j.ID, err
	}
	// no entitlement
	u1 := e.newUser()
	_, _, err := confirmM3(u1)
	jbsIs(t, "confirm with no entitlement", err, jobs.ErrNoEntitlement)
	// THE CONTROL: with an entitlement the same user's job confirms; module 3 holds no credits, so there is no reserve and no balance
	e.enableM3(u1, false)
	r, id, err := confirmM3(u1)
	mnyNoErr(t, "confirm with an entitlement", err)
	if r.Reserved != 0 || r.Pending != 4 || r.Job.Status != jobs.StatusConfirmed || e.reserveRows(id) != 0 {
		t.Errorf("a module 3 confirm: %+v with %d reserve rows, want 4 pending, nothing reserved", r, e.reserveRows(id))
	}
	if _, _, exists := e.bal(u1, "m3"); exists || e.int("SELECT count(*) FROM platform.ledger WHERE job_id = $1", id) != 0 {
		t.Error("module 3 confirm touched the ledger or made a balance")
	}
	// disabled
	u2 := e.newUser()
	e.enableM3(u2, false)
	e.exec(`INSERT INTO platform.ledger (user_id, module_id, kind, available_delta, reserved_delta, acting_admin_id, reason, idempotency_key)
		VALUES ($1, 'm3', 'm3_disable', 0, 0, $2, 'test disable', $3)`, u2, mnyAdminA, e.key())
	_, _, err = confirmM3(u2)
	jbsIs(t, "confirm with a disabled entitlement", err, jobs.ErrNoEntitlement)
	// expired by the database's clock
	u3 := e.newUser()
	e.enableM3(u3, true)
	_, _, err = confirmM3(u3)
	jbsIs(t, "confirm with an expired entitlement", err, jobs.ErrNoEntitlement)
	// suspended: a user the platform has observed suspended
	u4 := e.newUser()
	e.enableM3(u4, false)
	e.exec("UPDATE platform.m3_sync SET observed = 'suspended', observed_version = want_version WHERE user_id = $1", u4)
	_, _, err = confirmM3(u4)
	jbsIs(t, "confirm of a user observed suspended", err, jobs.ErrSuspended)
	// and the control: observed active, the same user confirms
	e.exec("UPDATE platform.m3_sync SET observed = 'active', observed_version = want_version WHERE user_id = $1", u4)
	if _, _, err = confirmM3(u4); err != nil {
		t.Errorf("a user observed active was refused: %v", err)
	}
	e.requireBooks("after the module 3 confirms")
}

func TestJobsStore_ModuleThreePreChecks(t *testing.T) {
	fake := &jbsFake{}
	e := newJbsEnv(t, jobs.WithPreChecks(fake))
	u1, u2, u3 := e.newUser(), e.newUser(), e.newUser()
	for _, u := range []string{u1, u2, u3} {
		e.enableM3(u, false)
	}

	// the module cannot run the campaign: ErrInfeasible with the module's causes, nothing written, and the module was asked with the job's id as the campaign and the pending count
	j1, p1 := e.previewed(u1, "m3", 4)
	fake.set(jobs.Feasibility{Feasible: false, Causes: []string{"no_account", "over_quota"}}, nil)
	before := e.writes()
	_, err := e.tryConfirm(u1, j1.ID, p1.Fingerprint, e.key(), nil)
	var inf jobs.ErrInfeasible
	if !errors.As(err, &inf) || !reflect.DeepEqual(inf.Causes, []string{"no_account", "over_quota"}) {
		t.Fatalf("an infeasible campaign: got %v, want ErrInfeasible with the two causes", err)
	}
	e.requireWrites("an infeasible campaign", before)
	if fake.count() != 1 || fake.calls[0] != (jbsCheckCall{u1, j1.ID, 4}) {
		t.Fatalf("the module was asked %v, want one call with the user, the job as the campaign and 4 recipients", fake.calls)
	}

	// the module cannot be asked: an immediate confirm is refused (no credits are held for a job that cannot start), a SCHEDULED one is not
	fake.set(jobs.Feasibility{Unavailable: true}, nil)
	_, err = e.tryConfirm(u1, j1.ID, p1.Fingerprint, e.key(), nil)
	jbsIs(t, "an immediate confirm while the module is unavailable", err, jobs.ErrModuleUnavailable)
	e.requireWrites("an immediate confirm while the module is unavailable", before)
	start := time.Now().Add(2 * time.Hour)
	r, err := e.tryConfirm(u1, j1.ID, p1.Fingerprint, e.key(), &start)
	mnyNoErr(t, "a scheduled confirm while the module is unavailable", err)
	if r.Job.Status != jobs.StatusScheduled {
		t.Errorf("the scheduled job is %s", r.Job.Status)
	}

	// feasible: the confirm goes through, and a replay of it does not ask the module again
	j2, p2 := e.previewed(u2, "m3", 3)
	fake.set(jobs.Feasibility{Feasible: true}, nil)
	n := fake.count()
	key := e.key()
	if _, err := e.tryConfirm(u2, j2.ID, p2.Fingerprint, key, nil); err != nil {
		t.Fatalf("a feasible campaign was refused: %v", err)
	}
	if fake.count() != n+1 {
		t.Errorf("a feasible confirm asked the module %d time(s)", fake.count()-n)
	}
	if r, err := e.tryConfirm(u2, j2.ID, p2.Fingerprint, key, nil); err != nil || !r.Replayed || fake.count() != n+1 {
		t.Errorf("a replay: %v, replayed %v, the module asked %d more time(s); want a replay that asks nothing", err, r.Replayed, fake.count()-n-1)
	}
	// a send through module 2 is not pre-checked by module 3
	e.fund(u2, "m2", 10)
	jm, pm := e.previewed(u2, "m2", 2)
	e.confirm(u2, jm.ID, pm.Fingerprint)
	if fake.count() != n+1 {
		t.Error("a module 2 confirm asked module 3")
	}

	// the module is never asked under a lock: while it is asked, the (user, module) advisory lock is free to take from another session
	j3, p3 := e.previewed(u3, "m3", 2)
	lockFree := false
	fake.probe = func() {
		var got bool
		if err := e.admin.QueryRow(testCtx(t), "SELECT pg_try_advisory_lock($1)", money.LockKey(u3, "m3")).Scan(&got); err != nil {
			t.Errorf("the probe: %v", err)
			return
		}
		lockFree = got
		if got {
			e.exec("SELECT pg_advisory_unlock($1)", money.LockKey(u3, "m3"))
		}
	}
	if _, err := e.tryConfirm(u3, j3.ID, p3.Fingerprint, e.key(), nil); err != nil {
		t.Fatalf("confirm with the probe: %v", err)
	}
	if !lockFree {
		t.Error("the (user, module) advisory lock was held while module 3 was asked: an HTTP call under the lock")
	}
	fake.probe = nil
	// a failure of the check itself is not a refusal of the request: it comes back as it is (and is not a typed refusal)
	boom := errors.New("the pre-check broke")
	u4 := e.newUser()
	e.enableM3(u4, false)
	j4, p4 := e.previewed(u4, "m3", 1)
	fake.set(jobs.Feasibility{}, boom)
	_, err = e.tryConfirm(u4, j4.ID, p4.Fingerprint, e.key(), nil)
	var notInfeasible jobs.ErrInfeasible
	if !errors.Is(err, boom) || errors.As(err, &notInfeasible) || errors.Is(err, jobs.ErrModuleUnavailable) {
		t.Errorf("a failing pre-check: got %v, want the check's own error", err)
	}
	e.requireBooks("after the pre-checks")
}

func TestJobsStore_ScheduledConfirm(t *testing.T) {
	e := newJbsEnv(t)
	A := mnyUserA
	e.fund(A, "m2", 1000)
	e.fund(A, "m1", 1000)

	j, p := e.previewed(A, "m2", 3)
	now := time.Now()
	before := e.writes()
	for name, start := range map[string]time.Time{
		"a start in the past":      now.Add(-time.Hour),
		"a start 30 seconds ahead": now.Add(30 * time.Second),
		"a start 367 days ahead":   now.Add(367 * 24 * time.Hour),
		"a start years ahead":      now.Add(5 * 366 * 24 * time.Hour),
	} {
		start := start
		_, err := e.tryConfirm(A, j.ID, p.Fingerprint, e.key(), &start)
		jbsBadRequest(t, name, err, "scheduled start")
	}
	e.requireWrites("every refused start (no reserve, the job still previewed)", before)
	// THE CONTROLS: two hours ahead is scheduled; the deadline is the start plus the max delay, by the database's clock
	start := now.Add(2 * time.Hour)
	r, err := e.tryConfirm(A, j.ID, p.Fingerprint, e.key(), &start)
	mnyNoErr(t, "a start two hours ahead", err)
	if r.Job.Status != jobs.StatusScheduled || r.Job.ScheduledStart == nil || r.Job.StartDeadline == nil || r.Reserved != 3 {
		t.Fatalf("the scheduled job is %+v reserved %d", r.Job, r.Reserved)
	}
	if d := r.Job.ScheduledStart.Sub(start); d < -time.Second || d > time.Second {
		t.Errorf("the stored start is %v, want %v", r.Job.ScheduledStart, start)
	}
	if !e.boolQ("SELECT start_deadline = scheduled_start + interval '30 minutes' FROM platform.jobs WHERE id = $1", j.ID) {
		t.Errorf("start_deadline is %v for a start at %v, want the start plus 30 minutes (the database added it)", r.Job.StartDeadline, r.Job.ScheduledStart)
	}
	// an immediate confirm starts as soon as it can: no scheduled start, and the deadline is confirmed_at plus the delay
	j2, p2 := e.previewed(A, "m1", 2)
	r2 := e.confirm(A, j2.ID, p2.Fingerprint)
	if r2.Job.Status != jobs.StatusConfirmed || r2.Job.ScheduledStart != nil || !e.boolQ("SELECT start_deadline = confirmed_at + interval '30 minutes' AND scheduled_start IS NULL FROM platform.jobs WHERE id = $1", j2.ID) {
		t.Errorf("the immediate confirm is %+v, want confirmed with no start and a deadline of confirmed_at + 30 minutes", r2.Job)
	}
	// the delay is the store's dial
	e45, err := jobs.New(e.pool, e.store, jobs.WithMaxStartDelay(45*time.Minute))
	mnyNoErr(t, "jobs.New with a 45 minute delay", err)
	B := mnyUserB
	e.fund(B, "m2", 10)
	jb, pb := e.previewed(B, "m2", 1)
	s3 := now.Add(300 * 24 * time.Hour) // far, but inside the 366 days
	if _, err := e45.Confirm(testCtx(t), B, jobs.ConfirmReq{JobID: jb.ID, Fingerprint: pb.Fingerprint, Key: e.key(), ScheduledStart: &s3}); err != nil {
		t.Fatalf("a start 300 days ahead was refused: %v", err)
	}
	if !e.boolQ("SELECT start_deadline = scheduled_start + interval '45 minutes' FROM platform.jobs WHERE id = $1", jb.ID) {
		t.Error("the deadline of a store with a 45 minute delay is not the start plus 45 minutes")
	}
	e.requireBooks("after the scheduled confirms")
}

// boolQ runs a one-value boolean query as the superuser.
func (e *jbsEnv) boolQ(sql string, args ...any) bool {
	e.t.Helper()
	var b bool
	if err := e.admin.QueryRow(testCtx(e.t), sql, args...).Scan(&b); err != nil {
		e.t.Fatalf("query failed: %v\n  sql: %s", err, mnyOneLine(sql))
	}
	return b
}

// Two confirms for the SAME user and module (two different previewed jobs) with a FORCED interleave: the test holds the (user, module) advisory lock, both confirms are started and PROVEN blocked
// on it (pg_stat_activity), then it lets go. The balance pays for both, so the slot is what decides: exactly one wins, the other is ErrSlotTaken, and the loser leaves no reserve and nothing else.
func TestJobsStore_ConcurrentConfirmsOfOneUserAndModuleHaveOneWinner(t *testing.T) {
	e := newJbsEnv(t)
	A := mnyUserA
	e.fund(A, "m2", 100)
	j1, p1 := e.previewed(A, "m2", 10)
	j2, p2 := e.previewed(A, "m2", 10)
	out := e.interleaveConfirms(A, "m2", [2]jbsCall{{j1.ID, p1.Fingerprint, e.key()}, {j2.ID, p2.Fingerprint, e.key()}})

	wins, losses := 0, 0
	var winner, loser string
	for i, o := range out {
		id := []string{j1.ID, j2.ID}[i]
		switch {
		case o.Err == nil:
			wins++
			winner = id
		case errors.Is(o.Err, jobs.ErrSlotTaken):
			losses++
			loser = id
		default:
			t.Fatalf("confirm %d: got %v, want success or ErrSlotTaken", i+1, o.Err)
		}
	}
	if wins != 1 || losses != 1 {
		t.Fatalf("%d confirm(s) won and %d lost the slot, want exactly one of each", wins, losses)
	}
	if e.jobStatus(winner) != "confirmed" || e.jobStatus(loser) != "previewed" {
		t.Errorf("the winner is %s and the loser %s, want confirmed and previewed", e.jobStatus(winner), e.jobStatus(loser))
	}
	// the loser leaves NO partial reserve: no ledger row, no price snapshot, no result, no suppression of its rows, no key
	if e.reserveRows(loser) != 0 || e.int("SELECT count(*) FROM platform.ledger WHERE job_id = $1", loser) != 0 || len(e.frozen(loser)) != 0 ||
		len(e.ruleCounts(loser)) != 0 || e.stateCounts(loser)[jobs.StatePending] != 10 || e.str("SELECT COALESCE(confirm_key, '') FROM platform.jobs WHERE id = $1", loser) != "" {
		t.Errorf("the loser left something behind: %d reserve rows, snapshot %v, results %v, rows %v", e.reserveRows(loser), e.frozen(loser), e.ruleCounts(loser), e.stateCounts(loser))
	}
	if n := e.int("SELECT count(*) FROM platform.ledger WHERE kind = 'reserve'"); n != 1 || e.reserveRows(winner) != 1 {
		t.Errorf("the ledger holds %d reserve rows, want exactly the winner's", n)
	}
	e.requireBal("one reserve of 10", A, "m2", 90, 10)
	e.requireBooks("after the race for the slot")
	// the slot is the winner's until it ends: the loser still cannot be confirmed (the legal twin is below, once the winner has gone)
	_, err := e.tryConfirm(A, loser, map[string]string{j1.ID: p1.Fingerprint, j2.ID: p2.Fingerprint}[loser], e.key(), nil)
	jbsIs(t, "the loser, asked again while the winner holds the slot", err, jobs.ErrSlotTaken)
	if _, err := e.jobs.CancelBeforeStart(testCtx(t), A, winner); err != nil {
		t.Fatalf("cancelling the winner: %v", err)
	}
	if _, err := e.tryConfirm(A, loser, map[string]string{j1.ID: p1.Fingerprint, j2.ID: p2.Fingerprint}[loser], e.key(), nil); err != nil {
		t.Errorf("the loser could not be confirmed once the slot was free: %v", err)
	}
	e.requireBooks("after the loser's turn")
}

// THE PAIRED CONTROL of the test above: the very same interleave on a database whose slot index has been dropped (a scratch database of the test's own, so nothing else is affected). Both confirms
// now win: it is the index, not the lock and not a check in Go, that makes one of them lose.
func TestJobsStore_ControlWithoutTheSlotIndexBothWin(t *testing.T) {
	e := newJbsEnv(t)
	A := mnyUserA
	e.fund(A, "m2", 100)
	j1, p1 := e.previewed(A, "m2", 10)
	j2, p2 := e.previewed(A, "m2", 10)
	e.exec("DROP INDEX platform.jobs_one_active_per_user_module")
	out := e.interleaveConfirms(A, "m2", [2]jbsCall{{j1.ID, p1.Fingerprint, e.key()}, {j2.ID, p2.Fingerprint, e.key()}})
	for i, o := range out {
		if o.Err != nil {
			t.Fatalf("confirm %d failed without the slot index: %v (the index is not what decided the race)", i+1, o.Err)
		}
	}
	if e.jobStatus(j1.ID) != "confirmed" || e.jobStatus(j2.ID) != "confirmed" || e.int("SELECT count(*) FROM platform.ledger WHERE kind = 'reserve'") != 2 {
		t.Errorf("jobs %s and %s, %d reserve rows: want both confirmed with two reserves", e.jobStatus(j1.ID), e.jobStatus(j2.ID), e.int("SELECT count(*) FROM platform.ledger WHERE kind = 'reserve'"))
	}
	e.requireBal("two reserves of 10", A, "m2", 80, 20)
	e.requireBooks("without the index (the money is still right: only the slot rule is gone)")
}

func TestJobsStore_CancelBeforeStartReleasesTheWholeReserve(t *testing.T) {
	e := newJbsEnv(t)
	A, B, C := mnyUserA, mnyUserB, mnyUserC
	e.fund(A, "m2", 100)
	e.fund(B, "m1", 100)

	// five sendable rows, two malformed lines and one repeat: 8 rows
	in := append(jbsAddrs(5, "c"), jobs.RowIn{Email: "not-an-address"}, jobs.RowIn{Email: ""}, jobs.RowIn{Email: "C00001@EXAMPLE.TEST"})
	j := e.createRows(A, "m2", in)
	p := e.preview(A, j.ID)
	r := e.confirm(A, j.ID, p.Fingerprint)
	if r.Reserved != 5 || r.Pending != 5 {
		t.Fatalf("confirm: %+v, want 5 credits for 5 rows", r)
	}
	e.requireBal("after the confirm", A, "m2", 95, 5)
	if rc := e.ruleCounts(j.ID); rc["P-08"] != 2 || rc["P-07"] != 1 || len(rc) != 2 {
		t.Fatalf("results after the confirm: %v, want two P-08 (malformed) and one P-07 (duplicate)", rc)
	}
	// a second job of the same user and module cannot take the slot while the first holds it (the index decides, and leaves nothing behind)
	j2, p2 := e.previewed(A, "m2", 2)
	before := e.writes()
	_, err := e.tryConfirm(A, j2.ID, p2.Fingerprint, e.key(), nil)
	jbsIs(t, "a second confirm while the first holds the slot", err, jobs.ErrSlotTaken)
	e.requireWrites("the refused second confirm", before)

	// a draft or a previewed job holds nothing: cancel-before-start is not for it
	_, err = e.jobs.CancelBeforeStart(testCtx(t), A, j2.ID)
	jbsNotAllowed(t, "CancelBeforeStart of a previewed job", err, "not_confirmed")

	got, err := e.jobs.CancelBeforeStart(testCtx(t), A, j.ID)
	mnyNoErr(t, "CancelBeforeStart", err)
	if got.Status != jobs.StatusCancelled || got.FinishedAt == nil {
		t.Fatalf("the cancelled job is %+v", got)
	}
	e.requireBal("the whole reserve came back", A, "m2", 100, 0)
	if e.int("SELECT count(*) FROM platform.ledger WHERE job_id = $1 AND kind = 'release'", j.ID) != 1 || e.int("SELECT available_delta FROM platform.ledger WHERE job_id = $1 AND kind = 'release'", j.ID) != 5 {
		t.Error("the ledger does not hold exactly one release of 5 credits")
	}
	// one result per row: the three the confirm wrote and one P-01 for each of the five rows that were to reach the module; none of them charged
	if rc := e.ruleCounts(j.ID); rc["P-01"] != 5 || rc["P-08"] != 2 || rc["P-07"] != 1 || len(rc) != 3 || e.int("SELECT count(*) FROM platform.job_results WHERE job_id = $1", j.ID) != 8 ||
		e.int("SELECT count(*) FROM platform.job_results WHERE job_id = $1 AND decision <> 'free'", j.ID) != 0 {
		t.Errorf("results after the cancel: %v, want 5 P-01, 2 P-08, 1 P-07, 8 in all, all free", e.ruleCounts(j.ID))
	}
	wantLast := "transition|confirmed>cancelled|user|" + A + "|cancelled by the user before it started"
	if lines := e.eventLines(j.ID); lines[len(lines)-1] != wantLast {
		t.Errorf("the last event is %q, want %q", lines[len(lines)-1], wantLast)
	}
	e.requireBooks("after the cancel")
	// a second cancel changes nothing (the job is cancelled and stays so)
	before = e.writes()
	if again, err := e.jobs.CancelBeforeStart(testCtx(t), A, j.ID); err != nil || again.Status != jobs.StatusCancelled {
		t.Errorf("a second cancel: %v, %s", err, again.Status)
	}
	e.requireWrites("a second cancel", before)
	// THE CONTROL: the slot is free again, and the job that was refused above confirms
	if _, err := e.tryConfirm(A, j2.ID, p2.Fingerprint, e.key(), nil); err != nil {
		t.Errorf("the second job could not be confirmed once the first was cancelled: %v", err)
	}

	// a scheduled job, paused, is cancelled the same way: the reserve of module 1 comes back
	jb, pb := e.previewed(B, "m1", 4)
	start := time.Now().Add(2 * time.Hour)
	_, err = e.tryConfirm(B, jb.ID, pb.Fingerprint, e.key(), &start)
	mnyNoErr(t, "a scheduled confirm", err)
	_, err = e.jobs.PauseScheduled(testCtx(t), B, jb.ID)
	mnyNoErr(t, "PauseScheduled", err)
	if _, err := e.jobs.CancelBeforeStart(testCtx(t), B, jb.ID); err != nil {
		t.Fatalf("cancelling a paused job: %v", err)
	}
	e.requireBal("a paused job's reserve came back", B, "m1", 100, 0)
	if rc := e.ruleCounts(jb.ID); rc["P-01"] != 4 || len(rc) != 1 {
		t.Errorf("results of the cancelled paused job: %v, want four P-01", rc)
	}
	// module 3 holds no credits: a cancel moves the job and records the results, and releases nothing
	e.enableM3(C, false)
	jc, pc := e.previewed(C, "m3", 3)
	e.confirm(C, jc.ID, pc.Fingerprint)
	if _, err := e.jobs.CancelBeforeStart(testCtx(t), C, jc.ID); err != nil {
		t.Fatalf("cancelling a module 3 job: %v", err)
	}
	if rc := e.ruleCounts(jc.ID); rc["P-01"] != 3 || e.int("SELECT count(*) FROM platform.ledger WHERE job_id = $1", jc.ID) != 0 {
		t.Errorf("the cancelled module 3 job has results %v and %d ledger rows, want three P-01 and none", rc, e.int("SELECT count(*) FROM platform.ledger WHERE job_id = $1", jc.ID))
	}
	e.requireBooks("after every cancel")
}

func TestJobsStore_PauseAndResumeAScheduledJob(t *testing.T) {
	e := newJbsEnv(t)
	A := mnyUserA
	e.fund(A, "m2", 100)
	j, p := e.previewed(A, "m2", 3)
	start := time.Now().Add(2 * time.Hour)
	_, err := e.tryConfirm(A, j.ID, p.Fingerprint, e.key(), &start)
	mnyNoErr(t, "a scheduled confirm", err)

	// a confirmed job cannot be paused: the DATABASE refuses (no such move), and the store reports its constraint
	e.fund(mnyUserB, "m1", 10)
	jc, pc := e.previewed(mnyUserB, "m1", 1)
	e.confirm(mnyUserB, jc.ID, pc.Fingerprint)
	_, err = e.jobs.PauseScheduled(testCtx(t), mnyUserB, jc.ID)
	jbsNotAllowed(t, "pausing a confirmed job", err, "job_transition_illegal")

	paused, err := e.jobs.PauseScheduled(testCtx(t), A, j.ID)
	mnyNoErr(t, "PauseScheduled", err)
	if paused.Status != jobs.StatusPaused || paused.PausedFrom != jobs.StatusScheduled {
		t.Fatalf("the paused job is %s (from %q)", paused.Status, paused.PausedFrom)
	}
	// nothing was submitted, so the slot and the reserve are held
	e.requireBal("a paused job holds its reserve", A, "m2", 97, 3)
	j2, p2 := e.previewed(A, "m2", 1)
	_, err = e.tryConfirm(A, j2.ID, p2.Fingerprint, e.key(), nil)
	jbsIs(t, "a second confirm while a job is paused", err, jobs.ErrSlotTaken)

	// resume goes back to scheduled
	_, err = e.jobs.ResumeScheduled(testCtx(t), A, j2.ID)
	jbsNotAllowed(t, "resuming a job that is not paused", err, "not_paused")
	resumed, err := e.jobs.ResumeScheduled(testCtx(t), A, j.ID)
	mnyNoErr(t, "ResumeScheduled", err)
	if resumed.Status != jobs.StatusScheduled || resumed.PausedFrom != "" {
		t.Errorf("the resumed job is %s (from %q)", resumed.Status, resumed.PausedFrom)
	}
	// a resume after the start deadline is refused: the engine expires such a job. (The deadline is moved with the lifecycle guard switched off for the one statement: no role can do it.)
	_, err = e.jobs.PauseScheduled(testCtx(t), A, j.ID)
	mnyNoErr(t, "pausing again", err)
	e.exec("ALTER TABLE platform.jobs DISABLE TRIGGER jobs_transition_guard")
	e.exec("UPDATE platform.jobs SET start_deadline = now() - interval '1 second' WHERE id = $1", j.ID)
	e.exec("ALTER TABLE platform.jobs ENABLE TRIGGER jobs_transition_guard")
	_, err = e.jobs.ResumeScheduled(testCtx(t), A, j.ID)
	jbsNotAllowed(t, "resuming after the start deadline", err, "start_deadline_passed")
	if e.jobStatus(j.ID) != "paused" {
		t.Errorf("a refused resume left the job %s", e.jobStatus(j.ID))
	}
	// the events of the whole life
	lines := e.eventLines(j.ID)
	want := []string{"transition|previewed>scheduled|user|" + A + "|scheduled by the user", "transition|scheduled>paused|user|" + A + "|paused by the user",
		"transition|paused>scheduled|user|" + A + "|resumed by the user", "transition|scheduled>paused|user|" + A + "|paused by the user"}
	if len(lines) != 2+len(want) || !reflect.DeepEqual(lines[2:], want) {
		t.Errorf("events %v, want after the first two %v", lines, want)
	}
	e.requireBooks("after pause and resume")
}

// What a module can do to a RUNNING job is the database's: the store only attempts the move and reports the database's refusal, with its constraint's name.
func TestJobsStore_TheModuleDecidesWhatARunningJobCanDo(t *testing.T) {
	e := newJbsEnv(t)
	A, B := mnyUserA, mnyUserB

	// module 1 has no per-job cancel: refused by the database, nothing changes
	j1, _ := e.confirmed(A, "m1", 3)
	jobWalkTo(t, e.admin, j1.ID, "running")
	before := e.writes()
	_, err := e.jobs.CancelBeforeStart(testCtx(t), A, j1.ID)
	jbsNotAllowed(t, "cancelling a running module 1 job", err, "job_module_cannot_cancel")
	e.requireWrites("the refused cancel of a running module 1 job", before)
	// no module can pause one job
	_, err = e.jobs.PauseScheduled(testCtx(t), A, j1.ID)
	jbsNotAllowed(t, "pausing a running module 1 job", err, "job_module_cannot_pause")
	e.requireWrites("the refused pause", before)

	// module 2 CAN be cancelled while running: the request is recorded (cancelling, with its time, its actor and its reason), nothing is released and no result is written: the engine finishes it
	j2, _ := e.confirmed(B, "m2", 4)
	jobWalkTo(t, e.admin, j2.ID, "running")
	got, err := e.jobs.CancelBeforeStart(testCtx(t), B, j2.ID)
	mnyNoErr(t, "cancelling a running module 2 job", err)
	if got.Status != jobs.StatusCancelling || got.CancelRequestedAt == nil {
		t.Errorf("the job is %s (cancel requested at %v), want cancelling with the time of the request", got.Status, got.CancelRequestedAt)
	}
	lines := e.eventLines(j2.ID)
	if last := lines[len(lines)-1]; last != "transition|running>cancelling|user|"+B+"|cancel requested by the user" {
		t.Errorf("the last event is %q", last)
	}
	if e.int("SELECT count(*) FROM platform.ledger WHERE job_id = $1 AND kind = 'release'", j2.ID) != 0 || len(e.ruleCounts(j2.ID)) != 0 {
		t.Error("a cancel request of a running job released credits or wrote results: that is the engine's, after the module has answered")
	}
	e.requireBal("the reserve of a cancelling job is still held", B, "m2", 996, 4)

	// THE CONTROL: the refusal is the capability's, not a Go if. Give module 2 a per-job pause and the same attempt succeeds; the job then resumes only where it paused from
	e.exec("UPDATE platform.modules SET can_pause = true WHERE id = 'm2'")
	C := mnyUserC
	j3, _ := e.confirmed(C, "m2", 2)
	jobWalkTo(t, e.admin, j3.ID, "running")
	paused, err := e.jobs.PauseScheduled(testCtx(t), C, j3.ID)
	mnyNoErr(t, "pausing a running job of a module that can", err)
	if paused.Status != jobs.StatusPaused || paused.PausedFrom != jobs.StatusRunning {
		t.Fatalf("the job is %s paused from %q", paused.Status, paused.PausedFrom)
	}
	// a job paused from running was submitted: the store does not resume it as a scheduled job (the database) and does not cancel it as if nothing was submitted
	_, err = e.jobs.ResumeScheduled(testCtx(t), C, j3.ID)
	jbsNotAllowed(t, "resuming a job that paused from running as a scheduled one", err, "job_resume_to_where_it_paused_from")
	_, err = e.jobs.CancelBeforeStart(testCtx(t), C, j3.ID)
	jbsNotAllowed(t, "cancelling a job that was submitted as if it had not started", err, "submitted_job_ends_through_the_engine")
	if e.int("SELECT count(*) FROM platform.ledger WHERE job_id = $1 AND kind = 'release'", j3.ID) != 0 {
		t.Error("a refused cancel released credits")
	}
	e.requireBooks("after the running-job refusals")
}

func TestJobsStore_DiscardAndReopen(t *testing.T) {
	e := newJbsEnv(t)
	A := mnyUserA
	e.fund(A, "m1", 100)

	// a draft and a previewed job are discarded by their user; they hold nothing, so no credit moves
	d := e.create(A, "m1", 2)
	got, err := e.jobs.DiscardDraft(testCtx(t), A, d.ID)
	mnyNoErr(t, "discarding a draft", err)
	if got.Status != jobs.StatusCancelled {
		t.Errorf("the discarded draft is %s", got.Status)
	}
	pv, pp := e.previewed(A, "m1", 2)
	if got, err := e.jobs.DiscardDraft(testCtx(t), A, pv.ID); err != nil || got.Status != jobs.StatusCancelled {
		t.Errorf("discarding a previewed job: %v, %s", err, got.Status)
	}
	wantLast := "transition|previewed>cancelled|user|" + A + "|discarded by the user"
	if lines := e.eventLines(pv.ID); lines[len(lines)-1] != wantLast {
		t.Errorf("the last event of a discarded previewed job is %q, want %q", lines[len(lines)-1], wantLast)
	}
	e.requireBal("a discarded job holds nothing", A, "m1", 100, 0)
	_ = pp

	// a confirmed job is NOT a draft: discarding it would strand its reserve, so the store refuses (CancelBeforeStart is the move that releases)
	c, _ := e.confirmed(A, "m2", 2)
	before := e.writes()
	_, err = e.jobs.DiscardDraft(testCtx(t), A, c.ID)
	jbsNotAllowed(t, "discarding a confirmed job", err, "not_a_draft")
	e.requireWrites("the refused discard", before)
	if _, err := e.jobs.CancelBeforeStart(testCtx(t), A, c.ID); err != nil {
		t.Errorf("the control: CancelBeforeStart of the same job: %v", err)
	}

	// reopen: a previewed job goes back to draft and loses its preview; the old fingerprint then confirms nothing; a new preview of the unchanged list has the same fingerprint
	r, p := e.previewed(A, "m1", 3)
	re, err := e.jobs.Reopen(testCtx(t), A, r.ID)
	mnyNoErr(t, "Reopen", err)
	if re.Status != jobs.StatusDraft || re.Preview != nil || re.PreviewFingerprint != "" || re.PreviewedAt != nil {
		t.Errorf("the reopened job is %+v, want a draft with no preview, fingerprint or time", re)
	}
	if e.str("SELECT COALESCE(preview::text, '') || COALESCE(preview_fingerprint, '') FROM platform.jobs WHERE id = $1", r.ID) != "" {
		t.Error("the preview columns were not cleared in the table")
	}
	lines := e.eventLines(r.ID)
	if last := lines[len(lines)-1]; last != "transition|previewed>draft|user|"+A+"|reopened by the user: the preview no longer describes the job" {
		t.Errorf("the last event of a reopened job is %q", last)
	}
	_, err = e.tryConfirm(A, r.ID, p.Fingerprint, e.key(), nil)
	jbsIs(t, "confirm of a reopened job", err, jobs.ErrNotPreviewed)
	if p2 := e.preview(A, r.ID); p2.Fingerprint != p.Fingerprint {
		t.Error("a new preview of an unchanged list has another fingerprint")
	}
	// the refusals: a draft is already a draft; a confirmed job cannot go back (the lifecycle's own refusal)
	_, err = e.jobs.Reopen(testCtx(t), A, e.create(A, "m1", 1).ID)
	jbsNotAllowed(t, "reopening a draft", err, "already_a_draft")
	cj, _ := e.confirmed(mnyUserB, "m1", 2)
	_, err = e.jobs.Reopen(testCtx(t), mnyUserB, cj.ID)
	jbsNotAllowed(t, "reopening a confirmed job", err, "job_transition_illegal")
	if e.jobStatus(cj.ID) != "confirmed" {
		t.Errorf("the refused reopen left the job %s", e.jobStatus(cj.ID))
	}
	e.requireBooks("after discard and reopen")
}

func TestJobsStore_EveryMoveWritesItsEvent(t *testing.T) {
	e := newJbsEnv(t)
	A, B := mnyUserA, mnyUserB
	e.fund(A, "m1", 100)
	e.fund(B, "m2", 100)

	j := e.create(A, "m1", 3)
	e.preview(A, j.ID)
	_, err := e.jobs.Reopen(testCtx(t), A, j.ID)
	mnyNoErr(t, "Reopen", err)
	p := e.preview(A, j.ID)
	start := time.Now().Add(2 * time.Hour)
	_, err = e.tryConfirm(A, j.ID, p.Fingerprint, e.key(), &start)
	mnyNoErr(t, "a scheduled confirm", err)
	_, err = e.jobs.PauseScheduled(testCtx(t), A, j.ID)
	mnyNoErr(t, "PauseScheduled", err)
	_, err = e.jobs.ResumeScheduled(testCtx(t), A, j.ID)
	mnyNoErr(t, "ResumeScheduled", err)
	_, err = e.jobs.CancelBeforeStart(testCtx(t), A, j.ID)
	mnyNoErr(t, "CancelBeforeStart", err)
	want := []string{
		"created|>|user|" + A + "|draft created",
		"transition|draft>previewed|user|" + A + "|preview requested by the user",
		"transition|previewed>draft|user|" + A + "|reopened by the user: the preview no longer describes the job",
		"transition|draft>previewed|user|" + A + "|preview requested by the user",
		"transition|previewed>scheduled|user|" + A + "|scheduled by the user",
		"transition|scheduled>paused|user|" + A + "|paused by the user",
		"transition|paused>scheduled|user|" + A + "|resumed by the user",
		"transition|scheduled>cancelled|user|" + A + "|cancelled by the user before it started",
	}
	if got := e.eventLines(j.ID); !reflect.DeepEqual(got, want) {
		t.Fatalf("the events of the job's life:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	// an immediate confirm and a discard of another job
	j2, p2 := e.previewed(B, "m2", 2)
	e.confirm(B, j2.ID, p2.Fingerprint)
	if lines := e.eventLines(j2.ID); lines[len(lines)-1] != "transition|previewed>confirmed|user|"+B+"|confirmed by the user" {
		t.Errorf("the confirm's event is %q", lines[len(lines)-1])
	}

	// the same events through the API: oldest first, with the actor, and paged by the last id seen
	all, err := e.jobs.Events(testCtx(t), A, j.ID, 0, 0)
	mnyNoErr(t, "Events", err)
	if len(all) != len(want) {
		t.Fatalf("Events returned %d, want %d", len(all), len(want))
	}
	for i, ev := range all {
		if ev.ActorKind != "user" || ev.ActingUserID != A || ev.ActingAdminID != "" || ev.JobID != j.ID || ev.Reason == "" || ev.CreatedAt.IsZero() || (i > 0 && ev.ID <= all[i-1].ID) {
			t.Errorf("event %d is %+v: want the user's, in order, with a reason and a time", i, ev)
		}
	}
	if all[0].Kind != "created" || all[0].FromStatus != "" || all[1].FromStatus != "draft" || all[1].ToStatus != "previewed" {
		t.Errorf("the first events are %+v and %+v", all[0], all[1])
	}
	var paged []int64
	after := int64(0)
	for {
		page, err := e.jobs.Events(testCtx(t), A, j.ID, after, 3)
		mnyNoErr(t, "Events page", err)
		if len(page) == 0 {
			break
		}
		if len(page) > 3 {
			t.Fatalf("a page of %d events with limit 3", len(page))
		}
		for _, ev := range page {
			paged = append(paged, ev.ID)
		}
		after = page[len(page)-1].ID
	}
	var ids []int64
	for _, ev := range all {
		ids = append(ids, ev.ID)
	}
	if !reflect.DeepEqual(paged, ids) {
		t.Errorf("paging by the last id gave %v, want %v", paged, ids)
	}
	// a job's events are its user's: another user gets ErrNotFound, and a limit beyond the maximum or a negative cursor is a bad request
	_, err = e.jobs.Events(testCtx(t), B, j.ID, 0, 0)
	jbsIs(t, "another user's events", err, jobs.ErrNotFound)
	_, err = e.jobs.Events(testCtx(t), A, j.ID, 0, jobs.MaxEvents+1)
	jbsBadRequest(t, "a limit over the maximum", err, "limit")
	_, err = e.jobs.Events(testCtx(t), A, j.ID, -1, 0)
	jbsBadRequest(t, "a negative cursor", err, "cursor")
	if evs, err := e.jobs.Events(testCtx(t), A, j.ID, all[len(all)-1].ID, 0); err != nil || len(evs) != 0 {
		t.Errorf("past the last event: %v, %d events, want none and no error", err, len(evs))
	}
}

func TestJobsStore_ListPagesByKeyset(t *testing.T) {
	e := newJbsEnv(t)
	A, B := mnyUserA, mnyUserB
	for i := 0; i < 7; i++ {
		e.create(A, "m1", 1)
	}
	e.create(B, "m1", 1)
	e.create(B, "m1", 1)
	want := e.rows("SELECT id::text FROM platform.jobs WHERE user_id = $1 ORDER BY created_at DESC, id DESC", A)
	if len(want) != 7 {
		t.Fatalf("setup: %d jobs for A", len(want))
	}
	var got []string
	cursor := ""
	pages := 0
	for {
		page, next, err := e.jobs.List(testCtx(t), A, jobs.ListOpts{After: cursor, Limit: 3})
		mnyNoErr(t, "List", err)
		pages++
		if len(page) > 3 {
			t.Fatalf("a page of %d with limit 3", len(page))
		}
		for _, j := range page {
			got = append(got, j.ID)
		}
		if next == "" {
			break
		}
		if len(page) != 3 {
			t.Fatalf("a short page of %d came with a next cursor", len(page))
		}
		cursor = next
	}
	if pages != 3 || !reflect.DeepEqual(got, want) {
		t.Errorf("%d pages gave %v, want the 7 jobs newest first %v in 3 pages (3, 3, 1)", pages, got, want)
	}
	// the default is 25, the maximum 100, a malformed cursor is a bad request, and the list is the user's own
	all, next, err := e.jobs.List(testCtx(t), A, jobs.ListOpts{})
	if err != nil || len(all) != 7 || next != "" {
		t.Errorf("the default page: %v, %d jobs, next %q; want all 7 and no next", err, len(all), next)
	}
	if _, _, err := e.jobs.List(testCtx(t), A, jobs.ListOpts{Limit: 100}); err != nil {
		t.Errorf("a limit of 100 was refused: %v", err)
	}
	_, _, err = e.jobs.List(testCtx(t), A, jobs.ListOpts{Limit: 101})
	jbsBadRequest(t, "a limit of 101", err, "limit")
	for _, bad := range []string{"!!!", "e30", "not a cursor"} {
		_, _, err = e.jobs.List(testCtx(t), A, jobs.ListOpts{After: bad})
		jbsBadRequest(t, fmt.Sprintf("the cursor %q", bad), err, "cursor")
	}
	// A's cursor on B's list is a position in time and id, never a way into A's rows
	bl, _, err := e.jobs.List(testCtx(t), B, jobs.ListOpts{After: cursor0(t, e, A)})
	mnyNoErr(t, "List(B) with A's cursor", err)
	ids := map[string]bool{}
	for _, id := range want {
		ids[id] = true
	}
	for _, j := range bl {
		if ids[j.ID] || j.UserID != B {
			t.Errorf("B's list holds %s of user %s", j.ID, j.UserID)
		}
	}
}

// cursor0 is the cursor after A's first page of 3 jobs.
func cursor0(t *testing.T, e *jbsEnv, user string) string {
	t.Helper()
	_, next, err := e.jobs.List(testCtx(t), user, jobs.ListOpts{Limit: 3})
	mnyNoErr(t, "List", err)
	if next == "" {
		t.Fatal("no next cursor")
	}
	return next
}
