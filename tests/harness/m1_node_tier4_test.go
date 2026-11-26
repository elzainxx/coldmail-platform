//go:build harness && m1node

package harness

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	m1ad "github.com/coldmail-platform/platform/internal/adapters/m1"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// The partial Tier 4 of module 1 (Phase 3 close-out A1, A3, D5; the owner's ruling OD-6/OD-8): describe, an idempotent job creation, totals on the job status, and a 400 for a job id that is not
// a UUID. The platform's OWN adapter against the real node stack: nothing is mocked.
//
//	TestM1Node_Tier4DescribeAndVocabulariesAreTheModulesOwn   the adapter ASKS module 1 what it may say (no constants stand in for the module any more): Describe names module verify, API v1 and the
//	                                                          capabilities the adapter relies on; CheckVocabularies finds every vocabulary the adapter handles with exactly the values it handles
//	                                                          (not Breaking, not Additive); a tenant whose key module 1 never learned is a typed 401, no value. Paired control: the report of the
//	                                                          same check against a describe with one value removed IS Breaking.
//	TestM1Node_Tier4IdempotentCreateAndTotals                 the same key and the same request give the SAME job (Replayed) and no second job exists in module 1's tables; another key is another
//	                                                          job; the same key with another request is refused (ErrIdempotencyKeyReused, Refused) and creates nothing; another tenant's identical key
//	                                                          is another job. The finished job's totals are submitted = settled = the rows, pending 0, by_status the rows counted by their verdict,
//	                                                          duplicates the plan's, malformed 0 (the platform filters), and all of it equals what Drain read row by row.
//	TestM1Node_Tier4ABadJobIDIsA400NotA500                    on the real module: a job id that is not a UUID is 400 validation_failed on both job routes (it was a 500); a UUID that names no job is
//	                                                          404 (the control), and the adapter surfaces the module's 400 as Refused, never Internal.

func m1tHTTP(t *testing.T, ctx context.Context, base, key, method, path string) (int, string, map[string]any) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(base, "/")+path, nil)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil // the key must never be handed to a proxy
	hc := &http.Client{Timeout: 30 * time.Second, Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("%s %s: no response", method, path) // never carries the URL or the key
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var doc map[string]any
	_ = json.Unmarshal(b, &doc)
	return resp.StatusCode, string(b), doc
}

func TestM1Node_Tier4DescribeAndVocabulariesAreTheModulesOwn(t *testing.T) {
	s := newM1eStack(t)
	a := s.newTenant("t4desc", true)
	c := s.newTenant("t4desc-stranger", false) // a real user whose key module 1 was never told about
	ctx, cancel := context.WithTimeout(s.ctx, 2*time.Minute)
	defer cancel()

	d, err := a.ad.Describe(ctx)
	if err != nil || d == nil {
		t.Fatalf("Describe through the adapter failed (%s): %v", modclient.KindOf(err), err)
	}
	if d.Module != "verify" || d.APIVersion != "v1" || !d.Capabilities["idempotent_bulk_create"] || !d.Capabilities["job_totals"] || d.Capabilities["quote"] || d.Capabilities["run"] {
		t.Errorf("describe = module %q, api %q, capabilities %v: want verify, v1, idempotent_bulk_create and job_totals true, quote and run false (module 1 stays a verifier)", d.Module, d.APIVersion, d.Capabilities)
	}
	if len(d.Raw) == 0 {
		t.Errorf("the describe carries no raw body: the module-specific keys (idempotency, routes) cannot be read")
	}
	rep, err := a.ad.CheckVocabularies(ctx)
	if err != nil {
		t.Fatalf("CheckVocabularies failed (%s): %v", modclient.KindOf(err), err)
	}
	// the seven verdict vocabularies are compared exactly. Module 1 also publishes four more words the adapter does not take from the describe: detail_issue (the issue words of a validation error) and, since the
	// Phase 4 close-out (module 1 migration 0020, the cancel of a bulk job), cancel_outcome, non_verdict_status and non_verdict_reason. All four are ADDITIVE, never breaking; the three new ones are checked here
	// against the words the adapter handles (m1ad.CancelOutcome*, m1ad.StatusCancelled, m1ad.ReasonCancelledByCaller), so a module that changes one of them is seen by this test and by the adapter's own decoding.
	wantExtra := []string{"cancel_outcome", "detail_issue", "non_verdict_reason", "non_verdict_status"}
	if rep.Breaking() || len(rep.Diffs) != 7 || len(rep.CodesMissing) != 0 || len(rep.CodesExtra) != 0 || !reflect.DeepEqual(rep.VocabulariesExtra, wantExtra) {
		t.Errorf("module 1's vocabularies against the adapter's: %+v (breaking %v, additive %v, %d diffs): want seven exact vocabularies, no code missing or extra, and exactly %v as the vocabularies the adapter does not compare", rep, rep.Breaking(), rep.Additive(), len(rep.Diffs), wantExtra)
	}
	for name, want := range map[string][]string{
		"cancel_outcome":     {m1ad.CancelOutcomeCancelled, m1ad.CancelOutcomeAlreadyCancelled, m1ad.CancelOutcomeAlreadyComplete},
		"non_verdict_status": {m1ad.StatusCancelled},
		"non_verdict_reason": {m1ad.ReasonCancelledByCaller},
	} {
		got := append([]string(nil), d.Vocabularies[name]...)
		w := append([]string(nil), want...)
		sort.Strings(got)
		sort.Strings(w)
		if !reflect.DeepEqual(got, w) {
			t.Errorf("module 1 publishes %s = %v, the adapter handles %v: a word was added, changed or removed", name, got, w)
		}
	}
	for _, df := range rep.Diffs {
		if !df.Exact() {
			t.Errorf("vocabulary %s is not exact: advertised %v, missing %v, extra %v", df.Name, df.Advertised, df.Missing, df.Extra)
		}
	}
	// the control: the same comparison on a describe with one value removed IS Breaking, so the clean answer above is not a check that cannot fail
	cut := *d
	cut.Vocabularies = modclient.Vocabularies{}
	for name, vals := range d.Vocabularies {
		cut.Vocabularies[name] = append([]string(nil), vals...)
	}
	if st := cut.Vocabularies["status"]; len(st) > 1 {
		cut.Vocabularies["status"] = st[1:]
	}
	if r2 := m1ad.CompareVocabularies(&cut); !r2.Breaking() {
		t.Errorf("a describe whose status vocabulary lost a value is not Breaking: the check cannot see the module change a word")
	}
	// a key module 1 never learned: a typed refusal and no value
	if dd, err := c.ad.Describe(ctx); err == nil || dd != nil || modclient.KindOf(err) != modclient.Refused {
		t.Errorf("an unprovisioned key's Describe = %v, %v: want no value and a Refused error (401)", dd, err)
	}
}

func TestM1Node_Tier4IdempotentCreateAndTotals(t *testing.T) {
	s := newM1eStack(t)
	a := s.newTenant("t4a", true)
	b := s.newTenant("t4b", true)
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Minute)
	defer cancel()

	addr1, addr2 := fresh("t4i")+"@nomx.fixture.test", fresh("t4i")+"@nomx.fixture.test"
	plan := m1ad.Prepare([]m1ad.Input{{ItemRef: "t4-1", Address: addr1}, {ItemRef: "t4-2", Address: addr2}, {ItemRef: "t4-3", Address: strings.ToUpper(addr1)}})
	if err := plan.Err(); err != nil || plan.Duplicates != 1 || len(plan.Items) != 3 {
		t.Fatalf("the plan is %d item(s) with %d duplicate(s) (err %v): this test needs 3 rows with exactly one repeat", len(plan.Items), plan.Duplicates, err)
	}
	other := m1ad.Prepare([]m1ad.Input{{ItemRef: "t4-x", Address: fresh("t4i") + "@nomx.fixture.test"}})
	key := m1eKey("t4idem")

	jobs0 := s.moduleJobCount()
	first, err := a.ad.Submit(ctx, plan, key)
	if err != nil || first.JobID == "" || first.Replayed {
		t.Fatalf("the first Submit = %+v, %v: want a new job (not a replay)", first, err)
	}
	if got := s.moduleJobCount(); got != jobs0+1 {
		t.Fatalf("module 1 holds %d job(s) after the first Submit, %d before: want exactly one more", got, jobs0)
	}
	// the same key and the same request: the same job, byte for byte the same counts, and NO second job
	again, err := a.ad.Submit(ctx, plan, key)
	if err != nil || !again.Replayed || again.JobID != first.JobID || again.Counts != first.Counts {
		t.Fatalf("the repeat = %+v, %v; the first = %+v: want the SAME job, the same counts and Replayed=true", again, err, first)
	}
	if got := s.moduleJobCount(); got != jobs0+1 {
		t.Fatalf("module 1 holds %d job(s) after the repeat, %d before the first: a repeat must create nothing", got, jobs0)
	}
	// the control: another key is another job
	second, err := a.ad.Submit(ctx, plan, m1eKey("t4idem-other"))
	if err != nil || second.Replayed || second.JobID == first.JobID {
		t.Fatalf("another key = %+v, %v: want another job (the repeat above must not be an accident of the stand-in)", second, err)
	}
	if got := s.moduleJobCount(); got != jobs0+2 {
		t.Fatalf("module 1 holds %d job(s) after the second key, want %d", got, jobs0+2)
	}
	// the same key with ANOTHER request: refused, and nothing is created
	_, err = a.ad.Submit(ctx, other, key)
	if !errors.Is(err, m1ad.ErrIdempotencyKeyReused) || modclient.KindOf(err) != modclient.Refused {
		t.Fatalf("the same key with another request = %v (kind %s): want ErrIdempotencyKeyReused, a Refused error", err, modclient.KindOf(err))
	}
	if got := s.moduleJobCount(); got != jobs0+2 {
		t.Fatalf("module 1 holds %d job(s) after the refused reuse, want %d: a refusal creates nothing", got, jobs0+2)
	}
	// another tenant's identical key is that tenant's own job: the scope of a key is its caller
	bj, err := b.ad.Submit(ctx, plan, key)
	if err != nil || bj.Replayed || bj.JobID == first.JobID {
		t.Fatalf("tenant B's Submit with A's key = %+v, %v: want B's own new job", bj, err)
	}

	// the totals of the finished job: equal to what was submitted and to what Drain read
	js, _, _ := s.waitComplete(a, first.JobID, len(plan.Items), 4*time.Minute)
	tot := js.Totals
	if tot.Submitted != 3 || tot.Settled != 3 || tot.Pending != 0 || tot.Duplicates != 1 || tot.Malformed != 0 || tot.ByStatus.Sum() != tot.Settled {
		t.Errorf("the totals of the finished job are %+v: want submitted 3, settled 3, pending 0, duplicates 1, malformed 0 and by_status adding up to the settled rows", tot)
	}
	rows, _ := s.drainAll(a.ad, first.JobID)
	var byStatus m1ad.StatusCounts
	for _, r := range rows {
		switch r.Status {
		case "valid":
			byStatus.Valid++
		case "invalid":
			byStatus.Invalid++
		case "risky":
			byStatus.Risky++
		case "unknown":
			byStatus.Unknown++
		default:
			t.Errorf("a row came back with the status %q", r.Status)
		}
	}
	if len(rows) != tot.Settled || byStatus != tot.ByStatus {
		t.Errorf("Drain read %d row(s) counted %+v; the totals say settled %d and by_status %+v: the totals must be the rows, counted", len(rows), byStatus, tot.Settled, tot.ByStatus)
	}
	// the replay's job is the finished job: its status is the same totals
	if js2, err := a.ad.Status(ctx, again.JobID); err != nil || js2.Totals != tot {
		t.Errorf("the replayed job's status = %+v, %v: want the totals %+v of the job it names", js2.Totals, err, tot)
	}
	// B's job is B's: A cannot read it (the module's own tenant isolation, already proved in B11; here only that the same key made two jobs)
	if _, err := a.ad.Status(ctx, bj.JobID); modclient.KindOf(err) != modclient.NotFound {
		t.Errorf("A's Status of B's job = %v (kind %s), want NotFound", err, modclient.KindOf(err))
	}
}

func TestM1Node_Tier4ABadJobIDIsA400NotA500(t *testing.T) {
	s := newM1eStack(t)
	a := s.newTenant("t4id", true)
	ctx, cancel := context.WithTimeout(s.ctx, 2*time.Minute)
	defer cancel()
	key := a.key()
	for _, c := range []struct{ what, path string }{
		{"the status of a job whose id is not a UUID", "/v1/jobs/not-a-uuid"},
		{"the results of a job whose id is not a UUID", "/v1/jobs/not-a-uuid/results"},
		{"a job id with a path trick in it", "/v1/jobs/..%2F..%2Fhealthz"},
	} {
		status, body, doc := m1tHTTP(t, ctx, s.base, key, http.MethodGet, c.path)
		code := ""
		if e, ok := doc["error"].(map[string]any); ok {
			code, _ = e["code"].(string)
		}
		if status != http.StatusBadRequest || code != "validation_failed" {
			t.Errorf("%s: HTTP %d code %q (%.160s): want 400 validation_failed (it was a 500)", c.what, status, code, body)
		}
	}
	// the control: a UUID that names no job is a plain 404 not_found, not a 400
	status, body, doc := m1tHTTP(t, ctx, s.base, key, http.MethodGet, "/v1/jobs/00000000-0000-4000-8000-000000000001")
	code := ""
	if e, ok := doc["error"].(map[string]any); ok {
		code, _ = e["code"].(string)
	}
	if status != http.StatusNotFound || code != "not_found" {
		t.Errorf("a UUID that names no job: HTTP %d code %q (%.160s): want 404 not_found", status, code, body)
	}
	// through the adapter: the module's 400 is a typed Refused or Invalid error, never Internal
	if _, err := a.ad.Status(ctx, "not-a-uuid"); err == nil || modclient.KindOf(err) == modclient.Internal {
		t.Errorf("the adapter's Status of a malformed id = %v (kind %s): want a typed refusal, never Internal", err, modclient.KindOf(err))
	}
}
