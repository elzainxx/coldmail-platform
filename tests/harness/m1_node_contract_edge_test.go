//go:build harness && m1node

package harness

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	m1ad "github.com/coldmail-platform/platform/internal/adapters/m1"
	"github.com/coldmail-platform/platform/internal/modclient"
	"github.com/coldmail-platform/platform/internal/norm"
)

// Phase 3, chunk B6c: the answers of module 1 that the end-to-end tests do not reach on their own, elicited from a test so that the contract drift gate (zz_m1_node_contract_drift_test.go,
// the last test of the binary) compares a REAL response with the recorded fixture of each. Every call goes through a client whose options carry the session's observer (the stack's
// adapters, and one raw modclient.Client over the same URL and the same tenant key for the requests the adapter would refuse before sending), asserts the module's own answer (status, error
// code, the details it names) so that the test fails loudly if the module says something else, prints no secret, and has a paired control: the same call with correct input succeeds.
//
//	POST /v1/verify                 ok                 the adapter's Verify on a fresh address of the node-backed recipe (valid.fixture.test), then the same address again, which module 1 answers from
//	                                                   its cache; the control is a malformed address, which is never sent
//	POST /v1/verify/bulk            validation_failed  a raw submission of an empty `items` list, and one whose two rows share an item_id; module 1 answers 400 and stores nothing; the control is a
//	                                                   valid two-row submission, 202
//	POST /v1/verify/bulk            payload_too_large  a raw submission of one row more than the cap the compose file gives the control plane (TM_QUEUE_BULK_MAX_ADDRESSES, the same variable
//	                                                   the test reads as HARNESS_M1NODE_BULK_CAP): 413 and nothing stored; the control is the first two of the same rows, 202
//	GET  /v1/jobs/{id}/results      empty_page         Results of a job whose rows sit at names the fixture's DNS never answers (2 s each), read at once after the submission: nothing has
//	                                                   settled yet, so the page is empty; the control is the same read after the job completes (two rows), and a page of one row with a cursor and the LAST page
//
// NOT HERE, because the end-to-end tests already reach them (B9 to B11): the accepted submission, the running and complete job, 401 and 404 on Status and Results, the bad cursor, the
// populated page. NOT REACHABLE, so listed with a reason in m1DriftNotElicited: submit paused, unavailable and internal_error.
//
// The three cases that need module 1 to fail on its own are not made to fail: module 1 answers `paused` only with its kill switch pulled (an environment variable read at boot),
// `unavailable` only when its bulk submitter is not wired, and `internal_error` only when a database write of its own fails.

const m1xEnvCap = "HARNESS_M1NODE_BULK_CAP" // the cap of rows per bulk submission the compose file gives the control plane; the test submits one more

// m1xBulkCap is the cap, refusing a value the tests of this binary could not live with (the pagination test submits 220 rows).
func m1xBulkCap(t *testing.T) int {
	t.Helper()
	n, err := strconv.Atoi(strings.TrimSpace(requireEnv(t, m1xEnvCap)))
	if err != nil || n < 300 || n > 5000 {
		t.Fatalf("%s must be an integer from 300 to 5000 (the cap of rows per submission set in docker-compose.m1node.yml: above the 220 rows of the pagination test, small enough to cross in one request), got %q", m1xEnvCap, strings.TrimSpace(requireEnv(t, m1xEnvCap)))
	}
	return n
}

// m1xRows are n distinct well-formed rows at the NXDOMAIN recipe's domain: nothing about them is ever verified when the request is refused.
func m1xRows(prefix, domain string, n int) []m1ad.Item {
	out := make([]m1ad.Item, n)
	for i := range out {
		out[i] = m1ad.Item{ItemID: fmt.Sprintf("%s-%05d", prefix, i), Address: fmt.Sprintf("%s-%05d@%s", prefix, i, domain)}
	}
	return out
}

// m1xRefusal says how err differs from the refusal the module must give: a *modclient.Error of kind Refused with this HTTP status and module error code, whose details name every
// (field, issue) pair of want. (An error that is not typed, or of another kind, is a difference of its own.)
func m1xRefusal(what string, err error, status int, code string, want ...modclient.Detail) []string {
	me, p := m1eWantErr(what, err, modclient.Refused, status, code)
	if me == nil {
		return p
	}
	for _, w := range want {
		found := false
		for _, d := range me.Details {
			if d == w {
				found = true
			}
		}
		if !found {
			p = append(p, fmt.Sprintf("%s: the details %v do not name %s: %s", what, me.Details, w.Field, w.Issue))
		}
	}
	return p
}

func (s *m1eStack) moduleJobsOf(userID string) int {
	s.t.Helper()
	var n int
	if err := s.admin.QueryRow(s.ctx, `SELECT count(*) FROM verify.jobs WHERE owner_user_id = $1::uuid`, userID).Scan(&n); err != nil {
		s.t.Fatalf("counting module 1's jobs of the tenant: %v", err)
	}
	return n
}

// TestM1Node_ContractEdgeCases: see the file comment.
func TestM1Node_ContractEdgeCases(t *testing.T) {
	s := newM1eStack(t)
	tn := s.newTenant("edge", true)
	raw, err := modclient.New(modclient.M1, s.base, tn.scope.M1Key, nil, modclient.Options{Timeout: 60 * time.Second, Observe: m1Drift.Observer()})
	if err != nil {
		t.Fatalf("modclient.New: %v", err) // never carries the URL or the key
	}
	submit := func(body any) (*modclient.Response, error) {
		return raw.Do(s.ctx, modclient.Request{Module: modclient.M1, Route: "m1.submit", Method: http.MethodPost, Path: "/v1/verify/bulk", Body: body, Expect: []int{http.StatusAccepted}, Timeout: 60 * time.Second})
	}
	jobOf := func(resp *modclient.Response) string {
		var w struct {
			JobID string `json:"job_id"`
		}
		if err := json.Unmarshal(resp.Body, &w); err != nil || w.JobID == "" {
			t.Fatalf("the 202 of the control submission carries no job_id: %v", err)
		}
		return w.JobID
	}
	nomx := m1eRecipe(s.m1nEnv, "no_mx").domain
	var problems []string
	add := func(p []string) { problems = append(problems, p...) }

	// 1. POST /v1/verify ok: the node-backed address, then the same address from the cache; the control: a malformed address is never sent
	addr := fresh("edge") + "@valid.fixture.test"
	cached := s.cacheRows("valid.fixture.test")
	v, err := tn.ad.Verify(s.ctx, addr)
	if err != nil || v.Malformed != "" || v.Status != "valid" || v.Reason != "accepted" || v.FromCache {
		t.Fatalf("Verify of a fresh address at the node-backed recipe gave %+v, %v; want valid/accepted, from the node (kind %v)", v, err, modclient.KindOf(err))
	}
	s.waitCacheRow("valid.fixture.test", cached)
	v2, err := tn.ad.Verify(s.ctx, addr)
	if err != nil || v2.Status != "valid" || v2.Reason != "accepted" || !v2.FromCache {
		t.Fatalf("Verify of the same address again gave %+v, %v; want valid/accepted answered from the cache", v2, err)
	}
	mv, err := tn.ad.Verify(s.ctx, "not-an-address")
	if err != nil || mv.Malformed != norm.ReasonNoAt || mv.Status != "" {
		t.Fatalf("the control: Verify of a malformed address gave %+v, %v; want it never sent (malformed %s, no verdict)", mv, err, norm.ReasonNoAt)
	}
	t.Logf("NOTE EDGE POST /v1/verify ok: a fresh address at the node-backed recipe answered valid/accepted (confidence %d, recommendation %s, from_cache=false), the same address again valid/accepted from_cache=%t; the control, a malformed address, was never sent (%s)", v.Confidence, v.Recommendation, v2.FromCache, mv.Malformed)

	// 2. POST /v1/verify/bulk validation_failed: refused before anything is stored
	jobs0 := s.moduleJobsOf(tn.userID)
	_, err = submit(map[string]any{"items": []m1ad.Item{}})
	add(m1xRefusal("an empty items list", err, http.StatusBadRequest, "validation_failed", modclient.Detail{Field: "items", Issue: "required"}))
	dup := []m1ad.Item{{ItemID: "edge-dup", Address: fresh("edge") + "@" + nomx}, {ItemID: "edge-dup", Address: fresh("edge") + "@" + nomx}}
	_, err = submit(map[string]any{"items": dup})
	add(m1xRefusal("two rows with one item_id", err, http.StatusBadRequest, "validation_failed", modclient.Detail{Field: "items[0].item_id", Issue: "duplicate"}, modclient.Detail{Field: "items[1].item_id", Issue: "duplicate"}))
	if n := s.moduleJobsOf(tn.userID); n != jobs0 {
		problems = append(problems, fmt.Sprintf("the refused submissions left module 1 with %d job(s) of the tenant, %d before: a 400 must store nothing", n, jobs0))
	}
	ctrl := m1xRows("edge-ok", nomx, 2)
	for i := range ctrl {
		ctrl[i].Address = fresh("edge") + "@" + nomx
	}
	resp, err := submit(map[string]any{"items": ctrl})
	if err != nil || resp.Status != http.StatusAccepted {
		t.Fatalf("the control: a valid two-row submission was refused (kind %v): %v", modclient.KindOf(err), err)
	}
	okJob := jobOf(resp)
	s.waitComplete(tn, okJob, len(ctrl), m1eJobWait)
	t.Logf("NOTE EDGE POST /v1/verify/bulk validation_failed: an empty items list and two rows sharing one item_id were each answered HTTP 400 validation_failed with the details named (items:required; items[0].item_id and items[1].item_id: duplicate); module 1 holds %d job(s) of the tenant before and after; the control, a valid two-row submission, was accepted (HTTP %d) and completed", jobs0, resp.Status)

	// 3. POST /v1/verify/bulk payload_too_large: one row more than the cap the control plane was started with
	capRows := m1xBulkCap(t)
	jobs1 := s.moduleJobsOf(tn.userID)
	_, err = submit(map[string]any{"items": m1xRows("edge-cap", nomx, capRows+1)})
	add(m1xRefusal(fmt.Sprintf("%d rows against a cap of %d", capRows+1, capRows), err, http.StatusRequestEntityTooLarge, "payload_too_large", modclient.Detail{Field: "items", Issue: "out_of_range"}))
	if n := s.moduleJobsOf(tn.userID); n != jobs1 {
		problems = append(problems, fmt.Sprintf("the refused oversize submission left module 1 with %d job(s) of the tenant, %d before: a 413 must store nothing, not a truncated job", n, jobs1))
	}
	small := m1xRows("edge-cap", nomx, 2)
	for i := range small {
		small[i].Address = fresh("edge") + "@" + nomx
	}
	resp, err = submit(map[string]any{"items": small})
	if err != nil || resp.Status != http.StatusAccepted {
		t.Fatalf("the control: two rows of the same submission were refused (kind %v): %v", modclient.KindOf(err), err)
	}
	s.waitComplete(tn, jobOf(resp), len(small), m1eJobWait)
	t.Logf("NOTE EDGE POST /v1/verify/bulk payload_too_large: %d rows against the control plane's cap of %d were answered HTTP 413 payload_too_large (items: out_of_range) and stored nothing (%d job(s) before and after); the control, two rows of the same submission, was accepted", capRows+1, capRows, jobs1)

	// 4. GET /v1/jobs/{id}/results empty_page: a job none of whose rows can have settled yet (each waits out the fixture's 2 s DNS silence), read at once
	var slow []m1eItem
	silent := m1eRecipe(s.m1nEnv, "dns_silent_own")
	for i := 0; i < 2; i++ {
		name := fmt.Sprintf("edgeslow%02d.%s", i, m1oZone)
		s.fx.setMode(name, "silent")
		local := fresh("edge")
		it := m1eNewRecipeItem(silent, local)
		it.address = local + "@" + name
		m1eAddItem(&slow, it)
	}
	slowJob := s.submitItems(tn, slow)
	t0 := time.Now()
	page, next, err := tn.ad.Results(s.ctx, slowJob, "", 0)
	took := time.Since(t0)
	if err != nil || len(page) != 0 || next != "" {
		t.Fatalf("Results read %v after the submission of a job whose rows wait out a 2 s DNS silence gave %d row(s), cursor %q, error %v: want an empty page and no cursor (a row settled in under 2 s: the silence did not hold)", took.Round(time.Millisecond), len(page), next, err)
	}
	js, err := tn.ad.Status(s.ctx, slowJob)
	if err != nil || js.Complete {
		t.Fatalf("the job is complete (%+v, %v) at the moment its first page was read empty", js, err)
	}
	s.waitComplete(tn, slowJob, len(slow), m1eJobWait)
	full, fnext, err := tn.ad.Results(s.ctx, slowJob, "", 0)
	if err != nil || len(full) != len(slow) || fnext != "" {
		t.Fatalf("the control: the same read after the job completed gave %d row(s), cursor %q, error %v; want the %d rows and no cursor", len(full), fnext, err, len(slow))
	}
	first, cursor, err := tn.ad.Results(s.ctx, slowJob, "", 1)
	if err != nil || len(first) != 1 || cursor == "" {
		t.Fatalf("a page of one row of the two gave %d row(s), cursor %q, error %v; want one row and a cursor", len(first), cursor, err)
	}
	last, lnext, err := tn.ad.Results(s.ctx, slowJob, cursor, 1)
	if err != nil || len(last) != 1 || lnext != "" {
		t.Fatalf("the LAST page gave %d row(s), cursor %q, error %v; want one row and no cursor", len(last), lnext, err)
	}
	t.Logf("NOTE EDGE GET /v1/jobs/{id}/results empty_page: read %v after the submission of a two-row job whose rows wait out the fixture's DNS silence, the page had 0 rows and no cursor and the job was not complete; the control, after completion: %d rows and no cursor; a page of 1 row with a cursor, then the last page of 1 row and no cursor", took.Round(time.Millisecond), len(full))

	if len(problems) != 0 {
		t.Fatalf("%d difference(s) between module 1's answers and what the edge cases expect:\n  %s", len(problems), strings.Join(problems, "\n  "))
	}
}
