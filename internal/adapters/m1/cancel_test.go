package m1

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/coldmail-platform/platform/internal/charge"
	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// THE BODIES IN THIS FILE ARE HAND-WRITTEN. Module 1's cancel (POST /v1/jobs/{id}/cancel, migration 0020, owner ruling C2) was added to the module after the recordings of testdata/contract
// were made, so there is no recorded body of it, of a status with `cancelled` keys or of a cancelled result row. Every shape below is written from the module's own contract
// (coldmail-verify Docs/api-contract.md, "Cancelling a job", "Job totals", "A cancelled row in the results") and from nothing else; the tests are named HandWritten so that they are found and
// replaced by recordings when the lead re-records the contracts after the pin (the route is declared in UncoveredRoutes until then). They prove how the adapter READS those documents, never
// that the module writes them.

// handStatus is a job-status body (and, with an outcome, the answer of the cancel route) with every number of ONE snapshot: submitted rows of which settled have a verdict (all `valid`),
// cancelled were cancelled and pending are still to be decided. The identity submitted = settled + cancelled + pending is the CALLER's to keep (a test that breaks it does so on purpose).
func handStatus(job, status string, complete bool, submitted, settled, cancelled, pending int, requested bool, outcome string) string {
	b := fmt.Sprintf(`{"job_id":%q,"status":%q,"complete":%t,"caller_ref":"platform-job-42",`+
		`"counts":{"total":%d,"settled":%d,"pending":%d,"cancelled":%d},`+
		`"totals":{"submitted":%d,"settled":%d,"pending":%d,"duplicates":0,"malformed":0,"cancelled":%d,"by_status":{"valid":%d,"invalid":0,"risky":0,"unknown":0}},`+
		`"processing_paused":false,"cancel_requested":%t`,
		job, status, complete, submitted, settled, pending, cancelled, submitted, settled, pending, cancelled, settled, requested)
	if outcome != "" {
		b += `,"cancel_outcome":` + strconv.Quote(outcome)
	}
	return b + "}"
}

// handRow is one element of the results listing written like the module's contract shows it, flags as given (a verdict's `"from_cache":false`, a cancelled row's none).
func handRow(status, reason, flags, itemID string, ordinal int) string {
	return `{"api_version":"v1","status":` + strconv.Quote(status) + `,"reason":` + strconv.Quote(reason) + `,"confidence":0,"recommendation":"review","email":"","domain":"acmecorp.com",` +
		`"flags":{` + flags + `},"signals":{},"meta":{"request_id":"","checked_at":"","latency_ms":0,"policy":"cold-email","policy_version":1,"verifier_ip":null,"node_id":null,"engine_version":null,"config_fingerprint":null,"from_cache":false},` +
		`"raw":null,"ordinal":` + strconv.Itoa(ordinal) + `,"item_id":` + strconv.Quote(itemID) + `}`
}

// handCancelledRow is a row cancelled before its check began, exactly as the contract's example shows it (an EMPTY flags object).
func handCancelledRow(ordinal int) string {
	return handRow("cancelled", "cancelled_by_caller", "", "row-"+strconv.Itoa(ordinal), ordinal)
}

// handVerdictRow is a verdict row (valid/accepted, uncached) of the same listing.
func handVerdictRow(ordinal int) string {
	return handRow("valid", "accepted", `"from_cache":false`, "row-"+strconv.Itoa(ordinal), ordinal)
}

// TestCancel_HandWritten_EachOutcomeIsReadAndTheRequestIsRight: POST /v1/jobs/{id}/cancel with the tenant's key, no body and no idempotency key (the route needs none), and each of the three
// outcome words of the contract is read with the job status of the same answer, exactly: this call cancelled the job (cancel_requested true, rows cancelled, the in-flight rows still pending),
// an earlier call had (a repeat changes nothing) and the job was already complete (nothing to cancel, cancel_requested stays false). Paired controls: a word outside the three, a word in
// another case, a missing word and a word of the wrong type are each a Contract error naming cancel_outcome and no value, so a reply the adapter does not understand is never read as "cancelled".
func TestCancel_HandWritten_EachOutcomeIsReadAndTheRequestIsRight(t *testing.T) {
	cases := []struct {
		outcome string
		body    string
		want    JobStatus
	}{
		{CancelOutcomeCancelled, handStatus(testJob, "running", false, 10, 4, 4, 2, true, "cancelled"), JobStatus{
			JobID: testJob, Status: "running", CancelRequested: true,
			Counts: JobCounts{Total: 10, Settled: 4, Pending: 2, Cancelled: 4},
			Totals: JobTotals{Submitted: 10, Settled: 4, Cancelled: 4, Pending: 2, ByStatus: StatusCounts{Valid: 4}},
		}},
		{CancelOutcomeAlreadyCancelled, handStatus(testJob, "complete", true, 10, 6, 4, 0, true, "already_cancelled"), JobStatus{
			JobID: testJob, Status: "complete", Complete: true, CancelRequested: true,
			Counts: JobCounts{Total: 10, Settled: 6, Cancelled: 4},
			Totals: JobTotals{Submitted: 10, Settled: 6, Cancelled: 4, ByStatus: StatusCounts{Valid: 6}},
		}},
		{CancelOutcomeAlreadyComplete, handStatus(testJob, "complete", true, 10, 10, 0, 0, false, "already_complete"), JobStatus{
			JobID: testJob, Status: "complete", Complete: true,
			Counts: JobCounts{Total: 10, Settled: 10},
			Totals: JobTotals{Submitted: 10, Settled: 10, ByStatus: StatusCounts{Valid: 10}},
		}},
	}
	for _, c := range cases {
		f := serve(t, 200, c.body)
		got, err := f.adapter(t).Cancel(testCtx(t), testJob)
		if err != nil || got.Outcome != c.outcome || got.Status != c.want {
			t.Errorf("%s: got %+v %v, want outcome %q and %+v", c.outcome, got, err, c.outcome, c.want)
			continue
		}
		if st := got.Status.Totals; st.Submitted != st.Settled+st.Cancelled+st.Pending || got.Status.Counts.Cancelled != st.Cancelled {
			t.Errorf("%s: the status read does not satisfy the identities it was checked by: %+v", c.outcome, got.Status)
		}
		r := f.seen()
		if len(r) != 1 || r[0].Method != http.MethodPost || r[0].Path != "/v1/jobs/"+testJob+"/cancel" || r[0].Auth != "Bearer "+f.key || len(r[0].Body) != 0 || r[0].IdemKey != "" {
			t.Errorf("%s: the request was %+v, want one POST of the job's cancel route with the key, no body and no idempotency key", c.outcome, r)
		}
	}
	good := cases[0].body
	for _, bad := range []struct {
		name, from, to string
	}{
		{"a word outside the three", `"cancel_outcome":"cancelled"`, `"cancel_outcome":"done"`},
		{"a word in another case", `"cancel_outcome":"cancelled"`, `"cancel_outcome":"Cancelled"`},
		{"an empty word", `"cancel_outcome":"cancelled"`, `"cancel_outcome":""`},
		{"a word of the wrong type", `"cancel_outcome":"cancelled"`, `"cancel_outcome":3`},
		{"no word at all", `,"cancel_outcome":"cancelled"`, ``},
		{"a null word", `"cancel_outcome":"cancelled"`, `"cancel_outcome":null`},
	} {
		body := strings.Replace(good, bad.from, bad.to, 1)
		if body == good {
			t.Fatalf("%s: the mutation changed nothing, so it proves nothing", bad.name)
		}
		got, err := serve(t, 200, body).adapter(t).Cancel(testCtx(t), testJob)
		if kind, key := kindAndKey(err); kind != modclient.Contract || key != "cancel_outcome" || got != (CancelResult{}) {
			t.Errorf("%s: got %+v kind %v key %q (%v), want no value and a Contract error naming cancel_outcome", bad.name, got, kind, key, err)
		}
	}
	// the status of the status route (no cancel_outcome) is not a cancel's answer: a cancel that is answered with it is refused, never read as a cancelled job
	got, err := serve(t, 200, handStatus(testJob, "running", false, 10, 4, 4, 2, true, "")).adapter(t).Cancel(testCtx(t), testJob)
	if kind, key := kindAndKey(err); kind != modclient.Contract || key != "cancel_outcome" || got != (CancelResult{}) {
		t.Errorf("a status body with no outcome: got %+v kind %v key %q (%v), want a Contract error naming cancel_outcome", got, kind, key, err)
	}
}

// TestCancel_HandWritten_ErrorsAreTheTypedErrorsOfTheRoute: the route is classified like every other route of the adapter. A job of another caller is the 404 of an absent job (NotFound,
// code not_found: never a 403), a refused key is Refused (unauthenticated), a store that cannot cancel answers 503 unavailable (Unavailable), a fault is Internal, and a 400 the module
// judges is Refused and sent once; each carries NO value (the zero CancelResult), and every error names the cancel route. The route is idempotent, so a 503 is retried per the dials (two
// failures then the answer succeeds on the third request) while a refusal never is. An id that is not a uuid is refused before any request. Paired control: the same stand-in answering the
// good body is read.
func TestCancel_HandWritten_ErrorsAreTheTypedErrorsOfTheRoute(t *testing.T) {
	const malformed = `{"error":{"code":"validation_failed","message":"One or more fields are invalid.","details":[{"field":"id","issue":"malformed"}],"request_id":"00000000-0000-7000-8000-0000000000d1"}}`
	for _, c := range []struct {
		name   string
		status int
		body   string
		kind   modclient.Kind
		code   string
	}{
		{"another caller's job", 404, fixture(t, "status-error-not-found.json"), modclient.NotFound, "not_found"},
		{"an unauthenticated caller", 401, fixture(t, "status-error-unauthenticated.json"), modclient.Refused, "unauthenticated"},
		{"a store that cannot cancel", 503, fixture(t, "submit-error-unavailable.json"), modclient.Unavailable, "unavailable"},
		{"a fault", 500, fixtureErr("internal_error"), modclient.Internal, "internal_error"},
		{"a refused id", 400, malformed, modclient.Refused, "validation_failed"},
	} {
		f := serve(t, c.status, c.body)
		got, err := f.adapterWith(t, modclient.Options{Sleep: noWait}).Cancel(testCtx(t), testJob)
		e := asModclient(err)
		if e == nil || e.Kind != c.kind || e.Route != routeCancel || (c.code != "" && e.Code != c.code) || got != (CancelResult{}) {
			t.Errorf("%s: got %+v %v, want a %v error of the cancel route with code %q and no value", c.name, got, err, c.kind, c.code)
		}
	}
	// a refusal is sent once whatever the dials say
	f := serve(t, 400, malformed)
	if _, err := f.adapterWith(t, modclient.Options{Retries: 2, Sleep: noWait}).Cancel(testCtx(t), testJob); modclient.KindOf(err) != modclient.Refused || f.count() != 1 {
		t.Errorf("a 400 with 2 retries: got %v after %d request(s), want Refused after exactly one", err, f.count())
	}
	// a 503 is retried (the route is idempotent: the module answers a repeat as already_cancelled)
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if n.Add(1) < 3 {
			w.WriteHeader(503)
			_, _ = w.Write([]byte(fixture(t, "submit-error-unavailable.json")))
			return
		}
		_, _ = w.Write([]byte(handStatus(testJob, "running", false, 10, 4, 4, 2, true, "cancelled")))
	}))
	defer srv.Close()
	a, err := New(srv.URL, config.Secret(newToken(t)), nil, modclient.Options{Retries: 2, Sleep: noWait})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := a.Cancel(testCtx(t), testJob); err != nil || got.Outcome != CancelOutcomeCancelled || n.Load() != 3 {
		t.Errorf("with 2 retries: %+v %v after %d request(s), want the answer on the third", got, err, n.Load())
	}
	// no request for an id that is not a uuid
	g := serve(t, 200, handStatus(testJob, "running", false, 10, 4, 4, 2, true, "cancelled"))
	for _, id := range []string{"", "J1", testJob + "0", "../" + testJob, testJob + "/cancel", "{" + testJob + "}"} {
		if _, err := g.adapter(t).Cancel(testCtx(t), id); modclient.KindOf(err) != modclient.Invalid {
			t.Errorf("Cancel(%q): got %v, want an Invalid error", id, err)
		}
	}
	if g.count() != 0 {
		t.Errorf("%d request(s) for ids that are not uuids", g.count())
	}
	if got, err := g.adapter(t).Cancel(testCtx(t), testJob); err != nil || got.Outcome != CancelOutcomeCancelled || g.count() != 1 {
		t.Errorf("control: the good answer gave %+v %v after %d request(s)", got, err, g.count())
	}
}

// TestCancel_HandWritten_TheAnswerIsJudgedExactlyAsAStatusIs: the status inside a cancel's answer goes through the SAME validation as GET /v1/jobs/{id} (judgeStatus), so a body whose numbers
// contradict themselves is refused by Cancel under the same key as by Status, and a cancel's answer is never read as "cancelled" over a status that cannot be believed. Each case mutates one
// thing of a good body (the same mutation helpers the status tests use) and is run through BOTH routes: the same Contract kind and key, each naming its own route. Paired control: the
// unmutated body is read by both.
func TestCancel_HandWritten_TheAnswerIsJudgedExactlyAsAStatusIs(t *testing.T) {
	good := handStatus(testJob, "running", false, 10, 3, 4, 3, true, "cancelled")
	goodStatus := handStatus(testJob, "running", false, 10, 3, 4, 3, true, "") // the status route's answer carries no outcome
	cases := []mutation{
		retype("totals.submitted", 11),
		retype("totals.pending", 2).naming("totals.submitted"),
		retype("totals.cancelled", 5).naming("totals.submitted"),
		retype("counts.cancelled", 3),
		retype("totals.cancelled", -1),
		retype("counts.cancelled", -2),
		retype("totals.by_status.valid", 7).naming("totals.by_status"), // a cancelled row counted as a verdict
		retype("counts.total", 11),
		retype("totals.cancelled", "4"),
		retype("totals.cancelled", nil),
		retype("counts.cancelled", 1.5),
		retype("cancel_requested", "yes"),
		retype("cancel_requested", nil),
		retype("status", "queued"),
		retype("complete", true).naming("status"), // complete beside `running`
		retype("job_id", "00000000-0000-4000-8000-0000000000ff"),
	}
	checked := 0
	for _, m := range cases {
		cb, sb := m.apply(t, good), m.apply(t, goodStatus)
		if cb == good || sb == goodStatus {
			t.Errorf("%s: the mutation changed nothing, so it proves nothing", m.name)
			continue
		}
		cr, cerr := serve(t, 200, cb).adapter(t).Cancel(testCtx(t), testJob)
		sr, serr := serve(t, 200, sb).adapter(t).Status(testCtx(t), testJob)
		ck, cKey := kindAndKey(cerr)
		sk, sKey := kindAndKey(serr)
		ce, se := asModclient(cerr), asModclient(serr)
		if ck != modclient.Contract || sk != modclient.Contract || cKey != m.key || sKey != m.key || cr != (CancelResult{}) || sr != (JobStatus{}) || ce == nil || se == nil || ce.Route != routeCancel || se.Route != routeStatus {
			t.Errorf("%s: cancel gave %+v kind %v key %q, status gave %+v kind %v key %q; want both a Contract error naming %q, each on its own route, and no value", m.name, cr, ck, cKey, sr, sk, sKey, m.key)
		}
		checked++
	}
	if checked < 15 {
		t.Fatalf("%d case(s) ran, want at least 15", checked)
	}
	cr, cerr := serve(t, 200, good).adapter(t).Cancel(testCtx(t), testJob)
	sr, serr := serve(t, 200, goodStatus).adapter(t).Status(testCtx(t), testJob)
	if cerr != nil || serr != nil || cr.Status != sr || cr.Outcome != CancelOutcomeCancelled {
		t.Fatalf("control: the unmutated body gave %+v %v (cancel) and %+v %v (status), want the same JobStatus from both", cr, cerr, sr, serr)
	}
}

// TestStatus_HandWritten_CancelledRowsAndTheirIdentities: a status whose job was cancelled reads every number into its own field (cancelled in the counts and in the totals, cancel_requested)
// and satisfies submitted = settled + cancelled + pending; a complete cancelled job (every row final, cancelled included) is read as complete with nothing pending. The paired controls break each
// identity in turn and are refused under the key of the number that is wrong: the OLD identity (submitted = settled + pending) no longer holds once a row is cancelled, the counts twin of
// cancelled must equal the totals', a by_status that counts the cancelled rows as verdicts is refused, a negative, null or retyped cancelled is a Contract error naming it.
func TestStatus_HandWritten_CancelledRowsAndTheirIdentities(t *testing.T) {
	read := func(body string) (JobStatus, error) {
		return serve(t, 200, body).adapter(t).Status(testCtx(t), testJob)
	}
	run := handStatus(testJob, "running", false, 10, 3, 4, 3, true, "")
	got, err := read(run)
	want := JobStatus{JobID: testJob, Status: "running", CancelRequested: true,
		Counts: JobCounts{Total: 10, Settled: 3, Pending: 3, Cancelled: 4},
		Totals: JobTotals{Submitted: 10, Settled: 3, Cancelled: 4, Pending: 3, ByStatus: StatusCounts{Valid: 3}}}
	if err != nil || got != want {
		t.Fatalf("a running cancelled job: got %+v %v, want %+v", got, err, want)
	}
	done, err := read(handStatus(testJob, "complete", true, 10, 6, 4, 0, true, ""))
	if err != nil || !done.Complete || done.Totals.Pending != 0 || done.Totals.Settled+done.Totals.Cancelled != done.Totals.Submitted || done.Totals.Cancelled != 4 {
		t.Fatalf("a complete cancelled job: got %+v %v, want complete with settled + cancelled = submitted and nothing pending", done, err)
	}
	for _, m := range []mutation{
		retype("totals.submitted", 11),
		retype("totals.cancelled", 5).naming("totals.submitted"),
		retype("totals.pending", 2).naming("totals.submitted"),
		retype("totals.settled", 4).naming("totals.submitted"),
		retype("counts.cancelled", 3),
		retype("counts.cancelled", 5),
		retype("totals.cancelled", -1),
		retype("counts.cancelled", -1),
		retype("totals.by_status.valid", 4).naming("totals.by_status"),
		retype("totals.by_status.valid", 7).naming("totals.by_status"),
		retype("totals.cancelled", "4"),
		retype("totals.cancelled", nil),
		retype("counts.cancelled", "4"),
		retype("cancel_requested", 1),
	} {
		mutated := m.apply(t, run)
		if mutated == run {
			t.Errorf("%s: the mutation changed nothing", m.name)
			continue
		}
		js, err := read(mutated)
		if kind, key := kindAndKey(err); kind != modclient.Contract || key != m.key || js != (JobStatus{}) {
			t.Errorf("%s: got %+v kind %v key %q (%v), want no value and a Contract error naming %q", m.name, js, kind, key, err, m.key)
		}
	}
	// the old identity: a body in which 4 rows are cancelled but submitted = settled + pending still holds (the module never serves it; an adapter that kept the old arithmetic would believe it)
	oldArithmetic := handStatus(testJob, "running", false, 10, 3, 4, 7, true, "")
	if js, err := read(oldArithmetic); modclient.KindOf(err) != modclient.Contract || js != (JobStatus{}) {
		t.Errorf("submitted = settled + pending with cancelled rows: got %+v %v, want a Contract error", js, err)
	}
}

// TestStatus_HandWritten_AnOldAnswerHasNoCancelledRows: the recordings of module 1 were made before its cancel, so their status bodies carry no `cancelled` key (neither in counts nor in
// totals) and no `cancel_requested`. An answer of that OLD shape still decodes, and the three read as 0, 0 and false: the new keys are additive. The old shape is built here from each
// recording by deleting those three keys if the file has them (so the test still describes the old answer after the lead re-records the contracts), and it must read exactly what the
// recording says without the adapter (recordedStatus); its paired control is the same job with the keys present and zero, which reads identically.
func TestStatus_HandWritten_AnOldAnswerHasNoCancelledRows(t *testing.T) {
	for _, name := range []string{"job-running.json", "job-complete.json"} {
		doc := recordedDoc(t, name)
		delete(obj(t, doc["counts"]), "cancelled")
		delete(obj(t, doc["totals"]), "cancelled")
		delete(doc, "cancel_requested")
		oldBody := encodeDoc(t, doc)
		for _, k := range []string{`"cancelled"`, `"cancel_requested"`} {
			if strings.Contains(oldBody, k) {
				t.Fatalf("%s: the old-shape body still holds %s, so it proves nothing", name, k)
			}
		}
		old, err := serve(t, 200, oldBody).adapter(t).Status(testCtx(t), testJob)
		want := recordedStatus(t, name)
		if err != nil || old.Counts.Cancelled != 0 || old.Totals.Cancelled != 0 || old.CancelRequested || old.Totals.Submitted == 0 || old.Counts.Total != want.Counts.Total || old.Totals != want.Totals {
			t.Fatalf("%s: got %+v %v, want cancelled 0, cancel_requested false and the numbers the recording says: %+v", name, old, err, want)
		}
		withZero := doc
		obj(t, withZero["counts"])["cancelled"] = 0
		obj(t, withZero["totals"])["cancelled"] = 0
		withZero["cancel_requested"] = false
		zeroBody := encodeDoc(t, withZero)
		if zeroBody == oldBody {
			t.Fatalf("%s: adding the keys changed nothing", name)
		}
		same, err := serve(t, 200, zeroBody).adapter(t).Status(testCtx(t), testJob)
		if err != nil || same != old {
			t.Fatalf("%s: with the keys present and zero got %+v %v, want exactly %+v", name, same, err, old)
		}
	}
}

// TestResults_HandWritten_CancelledRowsAreFreeAndVerdictRowsKeepTheirCacheSignal: a listing that mixes verdict rows and rows cancelled before their check began is read in full; a cancelled
// row (status cancelled, reason cancelled_by_caller, an EMPTY flags object) has no cache flag and the outcome the charge table's row M1-24 decides (free, no unit) whatever else it says (a
// cache flag a module put there is dropped), while the verdict rows are exactly what they were (valid/accepted charged by M1-03). Paired controls: a cancelled row with another reason, with
// none, a verdict status that carries the cancel's reason, the American spelling and a blank item_id are each a Contract error naming the key; and a VERDICT row without flags.from_cache is
// still refused naming results[i].flags.from_cache (the shape no longer requires the key of every row, so rowOf does for every verdict).
func TestResults_HandWritten_CancelledRowsAreFreeAndVerdictRowsKeepTheirCacheSignal(t *testing.T) {
	read := func(elems ...string) ([]Row, error) {
		rows, _, err := serve(t, 200, page(testJob, "", elems...)).adapter(t).Results(testCtx(t), testJob, "", 0)
		return rows, err
	}
	rows, err := read(handVerdictRow(0), handCancelledRow(1), handCancelledRow(2), handVerdictRow(3))
	if err != nil || len(rows) != 4 {
		t.Fatalf("a mixed listing: got %d row(s), %v", len(rows), err)
	}
	for i, r := range rows {
		cancelled := i == 1 || i == 2
		if (r.Status == StatusCancelled) != cancelled || r.Ordinal != i || r.ItemID != "row-"+strconv.Itoa(i) {
			t.Errorf("row %d read as %+v", i, r)
		}
		o := r.Outcome()
		d := charge.Decide(o)
		switch {
		case cancelled:
			if o != (charge.Outcome{Module: "m1", State: "cancelled", Reason: "cancelled_by_caller"}) || d.Kind != charge.Free || d.Rule != "M1-24" || d.Unit != "" || r.Reason != ReasonCancelledByCaller {
				t.Errorf("row %d (cancelled): outcome %+v decided {%v %s %q}, want module 1's two words alone, free by M1-24 with no unit", i, o, d.Kind, d.Rule, d.Unit)
			}
		default:
			if o.State != "valid" || o.Reason != "accepted" || d.Kind != charge.Charge || d.Rule != "M1-03" || d.Unit != "m1_own" {
				t.Errorf("row %d (verdict): outcome %+v decided {%v %s %q}, want the charge of M1-03", i, o, d.Kind, d.Rule, d.Unit)
			}
		}
	}
	if items, err := ToSettleItems(rows); err != nil || len(items) != 4 || items[1].Outcome != rows[1].Outcome() {
		t.Errorf("ToSettleItems of the mixed listing: %d item(s), %v", len(items), err)
	}
	// a cancelled row that also says it came from the cache is still the free row: the flag of a row that was never checked is dropped (M1-24 is first, and the outcome never carries it)
	cached, err := read(handRow("cancelled", "cancelled_by_caller", `"from_cache":true`, "row-0", 0))
	if err != nil || len(cached) != 1 || cached[0].Outcome().FromCache || charge.Decide(cached[0].Outcome()).Rule != "M1-24" {
		t.Errorf("a cancelled row with a cache flag: got %+v %v, want the free outcome with no cache flag", cached, err)
	}
	for _, c := range []struct {
		name, elem, key string
	}{
		{"a cancelled row with another reason", handRow("cancelled", "plausible", "", "row-0", 0), "results[0].reason"},
		{"a cancelled row with an empty reason", handRow("cancelled", "", "", "row-0", 0), "results[0].reason"},
		{"a verdict that carries the cancel's reason", handRow("valid", "cancelled_by_caller", `"from_cache":false`, "row-0", 0), "results[0].reason"},
		{"the American spelling of the status", handRow("canceled", "cancelled_by_caller", "", "row-0", 0), "results[0].status"},
		{"the status in another case", handRow("Cancelled", "cancelled_by_caller", "", "row-0", 0), "results[0].status"},
		{"a cancelled row with a blank item_id", handRow("cancelled", "cancelled_by_caller", "", "  ", 0), "results[0].item_id"},
		{"a verdict row with an empty flags object", handRow("valid", "accepted", "", "row-0", 0), "results[0].flags.from_cache"},
		{"a verdict row whose from_cache is not a boolean", handRow("risky", "plausible", `"from_cache":"no"`, "row-0", 0), "results[0].flags.from_cache"},
		{"a cancelled row whose from_cache is not a boolean", handRow("cancelled", "cancelled_by_caller", `"from_cache":1`, "row-0", 0), "results[0].flags.from_cache"},
	} {
		got, err := read(c.elem)
		if kind, key := kindAndKey(err); kind != modclient.Contract || key != c.key || got != nil {
			t.Errorf("%s: got %+v kind %v key %q (%v), want no rows and a Contract error naming %q", c.name, got, kind, key, err, c.key)
		}
	}
	// a bad cancelled row anywhere in a page fails the whole page: partial rows are never returned
	if got, err := read(handVerdictRow(0), handRow("cancelled", "plausible", "", "row-1", 1)); err == nil || got != nil {
		t.Errorf("a page with one bad cancelled row gave %+v %v, want the whole page refused", got, err)
	}
}

// TestDrain_HandWritten_ACancelledJobIsReadInFullAndNeverBelievedWithARowPending: a cancelled job's listing holds its verdict rows AND its cancelled rows, and a complete job is read in full
// when the rows delivered are settled + cancelled and that is every submitted row. The paired controls are the ways it must NOT drain: the listing without the cancelled rows (the totals
// say they exist), a job that says complete with a row still pending (the contradiction the platform never believes), and the arithmetic of a job that was never cancelled (cancelled 0,
// exactly as before). Each refusal is ErrIncomplete naming the numbers, including the cancelled count.
func TestDrain_HandWritten_ACancelledJobIsReadInFullAndNeverBelievedWithARowPending(t *testing.T) {
	run := func(status string, rows ...string) (DrainStats, error, int) {
		f := newFakeM1(t, func(c call) (int, string) {
			if strings.HasSuffix(c.Path, "/results") {
				return 200, page(testJob, "", rows...)
			}
			return 200, status
		})
		var delivered int
		st, err := f.adapter(t).Drain(testCtx(t), testJob, func(r []Row) error { delivered += len(r); return nil })
		return st, err, delivered
	}
	full := []string{handVerdictRow(0), handVerdictRow(1), handCancelledRow(2), handVerdictRow(3), handCancelledRow(4)}
	st, err, delivered := run(handStatus(testJob, "complete", true, 5, 3, 2, 0, true, ""), full...)
	if err != nil || delivered != 5 || st.Rows != 5 || st.CatchUp != 0 {
		t.Fatalf("a cancelled job read in full: %+v after %d row(s), %v", st, delivered, err)
	}
	for _, c := range []struct {
		name   string
		status string
		rows   []string
		want   []string
	}{
		{"the cancelled rows are missing from the listing", handStatus(testJob, "complete", true, 5, 3, 2, 0, true, ""), []string{handVerdictRow(0), handVerdictRow(1), handVerdictRow(3)}, []string{"3 row(s) were read", "total 5", "settled 3", "cancelled 2"}},
		{"complete with a row still pending", handStatus(testJob, "complete", true, 5, 3, 1, 1, true, ""), []string{handVerdictRow(0), handVerdictRow(1), handCancelledRow(2), handVerdictRow(3)}, []string{"4 row(s) were read", "total 5", "pending 1", "cancelled 1"}},
		{"a listing longer than settled + cancelled", handStatus(testJob, "complete", true, 5, 3, 2, 0, true, ""), append(append([]string(nil), full...), handCancelledRow(5)), []string{"6 row(s) were read", "total 5"}},
	} {
		st, err, delivered := run(c.status, c.rows...)
		if !errors.Is(err, ErrIncomplete) || modclient.KindOf(err) != modclient.Contract {
			t.Errorf("%s: got %v (kind %v), want a Contract error with cause ErrIncomplete", c.name, err, modclient.KindOf(err))
			continue
		}
		for _, w := range c.want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%s: the error %q does not say %q", c.name, err, w)
			}
		}
		if delivered != st.Rows {
			t.Errorf("%s: %d row(s) reached fn but the stats say %d", c.name, delivered, st.Rows)
		}
	}
	// control: a job never cancelled drains by the arithmetic it always had (cancelled 0)
	if st, err, delivered := run(handStatus(testJob, "complete", true, 3, 3, 0, 0, false, ""), handVerdictRow(0), handVerdictRow(1), handVerdictRow(2)); err != nil || delivered != 3 || st.Rows != 3 {
		t.Errorf("control: a job that was never cancelled: %+v after %d row(s), %v", st, delivered, err)
	}
}

// TestNonVerdictVocabListsAreTheSwitches: the two non-verdict words (status `cancelled`, reason `cancelled_by_caller`) and the three cancel outcomes are each held twice in vocab.go, as a list
// and as the cases of a known* switch, and the two are the same set in both directions, read from the source of vocab.go (the way TestVocabListsAreTheSwitches holds the seven verdict sets).
// They are NOT among the seven sets CheckVocabularies compares with the module's describe: no verdict list holds them and vocabSets() (that comparison's input) lacks them, because a
// module without cancel publishes no such set and its absence is not a break. Paired controls on mutated source text: a case added to a switch and a case removed are each reported.
func TestNonVerdictVocabListsAreTheSwitches(t *testing.T) {
	src, err := os.ReadFile("vocab.go")
	if err != nil {
		t.Fatal(err)
	}
	pairs := []struct {
		fn   string
		list []string
	}{
		{"knownNonVerdictStatus", nonVerdictStatusValues},
		{"knownNonVerdictReason", nonVerdictReasonValues},
		{"knownCancelOutcome", cancelOutcomeValues},
	}
	diff := func(src []byte, fn string, list []string) []string {
		cases, err := switchCases(src, fn)
		if err != nil {
			return []string{err.Error()}
		}
		a, b := sortedCopy(cases), sortedCopy(list)
		var out []string
		for _, v := range b {
			if !containsString(a, v) {
				out = append(out, fmt.Sprintf("%s: the list holds %q and the switch does not handle it", fn, v))
			}
		}
		for _, v := range a {
			if !containsString(b, v) {
				out = append(out, fmt.Sprintf("%s: the switch handles %q and the list does not hold it", fn, v))
			}
		}
		return out
	}
	for _, p := range pairs {
		if d := diff(src, p.fn, p.list); len(d) != 0 {
			t.Errorf("%v", d)
		}
	}
	if len(nonVerdictStatusValues) != 1 || len(nonVerdictReasonValues) != 1 || len(cancelOutcomeValues) != 3 {
		t.Fatalf("a vocabulary changed size: %d non-verdict statuses, %d non-verdict reasons, %d cancel outcomes (module 1's contract: 1, 1 and 3)", len(nonVerdictStatusValues), len(nonVerdictReasonValues), len(cancelOutcomeValues))
	}
	if nonVerdictStatusValues[0] != StatusCancelled || nonVerdictReasonValues[0] != ReasonCancelledByCaller || !reflect.DeepEqual(cancelOutcomeValues, []string{CancelOutcomeCancelled, CancelOutcomeAlreadyCancelled, CancelOutcomeAlreadyComplete}) {
		t.Fatalf("the exported constants and the lists differ: %v %v %v", nonVerdictStatusValues, nonVerdictReasonValues, cancelOutcomeValues)
	}
	// not verdict words, and not in the sets the describe comparison reads
	for _, v := range append(append([]string(nil), nonVerdictStatusValues...), nonVerdictReasonValues...) {
		if containsString(statusValues, v) || containsString(reasonValues, v) {
			t.Errorf("%q is a verdict word: a non-verdict word must never be one (a verdict is never cancelled)", v)
		}
	}
	if len(vocabSets()) != 7 {
		var names []string
		for k := range vocabSets() {
			names = append(names, k)
		}
		sort.Strings(names)
		t.Errorf("vocabSets() holds %d sets %v, want the seven that CheckVocabularies compares with the describe of the module", len(names), names)
	}
	if knownStatus("cancelled") || knownReason("cancelled_by_caller") || !knownNonVerdictStatus("cancelled") || !knownNonVerdictReason("cancelled_by_caller") || knownNonVerdictStatus("valid") || knownNonVerdictReason("accepted") {
		t.Error("the known* switches confuse the verdict words and the non-verdict words")
	}

	// controls
	added := bytes.Replace(src, []byte("case \"cancelled\":\n\t\treturn true\n\t}\n\treturn false\n}\n\nfunc knownNonVerdictReason"), []byte("case \"cancelled\", \"canceled\":\n\t\treturn true\n\t}\n\treturn false\n}\n\nfunc knownNonVerdictReason"), 1)
	if bytes.Equal(added, src) {
		t.Fatal("control setup: the switch of knownNonVerdictStatus is not where the control expects it")
	}
	if d := diff(added, "knownNonVerdictStatus", nonVerdictStatusValues); len(d) != 1 || !strings.Contains(d[0], `the switch handles "canceled"`) {
		t.Fatalf("control: a case added to the switch gave %v, want exactly that case reported", d)
	}
	removed := bytes.Replace(src, []byte(`case "cancelled", "already_cancelled", "already_complete":`), []byte(`case "cancelled", "already_cancelled":`), 1)
	if bytes.Equal(removed, src) {
		t.Fatal("control setup: the switch of knownCancelOutcome is not where the control expects it")
	}
	if d := diff(removed, "knownCancelOutcome", cancelOutcomeValues); len(d) != 1 || !strings.Contains(d[0], `the list holds "already_complete"`) {
		t.Fatalf("control: a case removed from the switch gave %v, want that value reported", d)
	}
}

func containsString(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}
