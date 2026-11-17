//go:build harness && m2worker

package harness

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"
)

// Phase 3, chunk B8: module 2 REALLY sends in the harness. A real worker (docker-compose.m2worker.yml, run.sh stage 5c) takes module 2's jobs and talks SMTP to the controllable sink
// (tests/harness/sink), which can accept, reject (550) or defer (451) a named recipient at RCPT TO and hold the final reply. The tests call module 2's own routes (quote, run, report, cancel; pools,
// accounts and config through the operator surface) and compare what module 2 REPORTS with what the sink independently SAW. Helpers: m2_worker_helpers_test.go.
//
// Four outcomes must really happen, each observed through module 2's own report:
//   sent                        a recipient the sink accepts: state sent, a provider message id, the sink holds exactly one message
//   bounced                     a recipient the sink rejects with 550 at RCPT TO: state bounced / bounce_permanent, tried ONCE, no message
//   failed / attempts_exhausted a recipient the sink defers with 451: retried, then failed (two deferrals at the shortened cap), no message
//   cancelled                   a campaign cancelled while one item is in flight and the rest still queued: nothing new reaches the sink once the cancel has returned
// Every scenario has a paired control, the same flow without the fault: all sent. The tests run in this order and the held-send test runs LAST (one worker takes one job at a time, so a held send
// stalls everything queued behind it).

// m2wSinkWant is what the sink must say about one recipient: the messages it accepted and its answers at RCPT TO.
type m2wSinkWant struct{ Messages, Accepted, Rejected, Deferred int }

var (
	m2wWantSent      = m2wWant{State: "sent", Reason: m2wNull, ErrorClass: m2wNull, PMID: true}
	m2wWantBounced   = m2wWant{State: "bounced", Reason: "bounce_permanent", ErrorClass: "bounce_permanent"}
	m2wWantExhausted = m2wWant{State: "failed", Reason: "attempts_exhausted", ErrorClass: "transport_4xx"}
	m2wWantCancelled = m2wWant{State: "cancelled", Reason: "cancelled", ErrorClass: m2wAny}
)

// checkOutcomes compares, for every item, BOTH of module 2's reports (the campaign report's row and the single-job report) with what the scenario expects of an item of that kind, and the two with each
// other. Divergences are t.Errorf, not t.Fatalf: one run shows every one of them.
func (r *m2wRun) checkOutcomes(t *testing.T, label string, tn *m2wTenant, rep m2wCampaign, want func(it *m2wItem) m2wWant) {
	t.Helper()
	byItem := rep.byItem()
	for _, it := range tn.Items {
		w := want(it)
		what := fmt.Sprintf("%s: %s item %02d (%s, %s)", label, tn.Name, it.Index, it.Kind, it.Email)
		camp, ok := byItem[it.ID]
		if !ok {
			t.Errorf("%s: the campaign report does not list the item", what)
			continue
		}
		r.checkRow(t, what+" in the campaign report", camp, w)
		single := r.singleTerminal(t, tn, it, 20*time.Second)
		r.checkRow(t, what+" in the single-job report", single, w)
		if single.JobID != it.JobID {
			t.Errorf("%s: the single-job report names the job %s, POST /v1/run answered %s", what, single.JobID, it.JobID)
		}
		if single.State != camp.State || single.Reason != camp.Reason && !(single.Reason == m2wNull && camp.Reason == m2wAbsent) || single.ErrorClass != camp.ErrorClass {
			t.Errorf("%s: the two reports disagree: campaign report (%s), single-job report (%s)", what, camp, single)
		}
		t.Logf("NOTE report %s item %02d %-6s %s: %s", tn.Name, it.Index, it.Kind, it.Email, single)
	}
}

// checkPartition asserts the exact set equality the platform will rely on: the items module 2 reports in the given terminal states, together, are exactly the items that were submitted (no item
// missing, none invented, none twice), and the walk holds exactly as many rows as items were submitted.
func (r *m2wRun) checkPartition(t *testing.T, label string, tn *m2wTenant, rep m2wCampaign, states ...string) {
	t.Helper()
	in := map[string]bool{}
	for _, s := range states {
		in[s] = true
	}
	var got []string
	for _, row := range rep.Rows {
		if in[row.State] {
			got = append(got, row.ItemID)
		}
	}
	sort.Strings(got)
	onlyReported, onlySubmitted := m2wDiff(got, tn.ids())
	if len(onlyReported) > 0 || len(onlySubmitted) > 0 || len(got) != len(tn.Items) || len(rep.Rows) != len(tn.Items) {
		t.Errorf("%s: %s: the items reported %s (%d) are not exactly the %d submitted: reported but never submitted %v, submitted but not reported %v (the campaign walk holds %d row(s))",
			label, tn.Name, strings.Join(states, "+"), len(got), len(tn.Items), onlyReported, onlySubmitted, len(rep.Rows))
	}
}

// checkTotals compares module 2's campaign totals with the expected numbers; the seven numbers of module 2's partition must add up to total.
func (r *m2wRun) checkTotals(t *testing.T, label string, tn *m2wTenant, rep m2wCampaign, want m2wTotals) {
	t.Helper()
	got := rep.Totals
	if got != want {
		t.Errorf("%s: %s: the campaign totals are (%s), want (%s)", label, tn.Name, got, want)
	}
	if sum := got.Sent + got.Bounced + got.Failed + got.InFlight + got.Pending + got.Waiting + got.Cancelled; sum != got.Total {
		t.Errorf("%s: %s: module 2's seven totals add up to %d, not to total %d (they must partition it): %s", label, tn.Name, sum, got.Total, got)
	}
	if !rep.IsComplete || rep.Terminal != len(tn.Items) {
		t.Errorf("%s: %s: progress says is_complete=%v terminal=%d, want true and %d", label, tn.Name, rep.IsComplete, rep.Terminal, len(tn.Items))
	}
}

// checkSink compares, for every recipient of the tenant, the sink's own counters with what the scenario expects: how many messages it accepted and how it answered at RCPT TO. A sent item's message
// is also tied to ITS job: the Message-ID module 2 builds is <job id@sending domain>, so it must contain the job id module 2 answered to POST /v1/run.
func (r *m2wRun) checkSink(t *testing.T, label string, tn *m2wTenant, v m2wView, want func(it *m2wItem) m2wSinkWant) {
	t.Helper()
	for _, it := range tn.Items {
		w := want(it)
		c := v.Rcpt[it.Email]
		what := fmt.Sprintf("%s: %s item %02d (%s, %s)", label, tn.Name, it.Index, it.Kind, it.Email)
		if got := v.messages(it.Email); got != w.Messages {
			t.Errorf("%s: the sink holds %d message(s), want %d (a recipient that was sent is sent exactly once; one that was bounced or exhausted has none)", what, got, w.Messages)
		}
		if c.Accepted != w.Accepted || c.Rejected != w.Rejected || c.Deferred != w.Deferred {
			t.Errorf("%s: at RCPT TO the sink answered accepted=%d rejected=%d deferred=%d, want accepted=%d rejected=%d deferred=%d", what, c.Accepted, c.Rejected, c.Deferred, w.Accepted, w.Rejected, w.Deferred)
		}
		for _, m := range v.Msgs[it.Email] {
			if len(m.To) != 1 || strings.ToLower(m.To[0]) != it.Email {
				t.Errorf("%s: the sink's message is addressed to %v, want exactly this recipient", what, m.To)
			}
			if it.JobID != "" && !strings.Contains(strings.ToLower(m.MessageID), strings.ToLower(it.JobID)) {
				t.Errorf("%s: the sink's message has the Message-ID %q, which does not contain the job id %s module 2 answered to POST /v1/run (module 2 builds <job id@sending domain>): the message is not this job's", what, m.MessageID, it.JobID)
			}
			t.Logf("NOTE sink message %s item %02d %s: seq %d, from %q, subject %q, message-id %q, held %d ms", tn.Name, it.Index, it.Email, m.Seq, m.From, m.Subject, m.MessageID, m.HeldMs)
		}
	}
}

// checkPaging reads the finished campaign again in pages of three and compares the walk with the single page of 200 the test polled with: the same items, once each, in the order they were run, every page but
// the last holding three and the last one's next_cursor null (the walk ends on it).
func (r *m2wRun) checkPaging(t *testing.T, label string, tn *m2wTenant, full m2wCampaign) {
	t.Helper()
	small := r.campaign(t, tn.UserID, tn.CampaignID, 3)
	if small.PageSize != 3 {
		t.Errorf("%s: %s: module 2 echoed a page size of %d for a request of 3", label, tn.Name, small.PageSize)
	}
	if want := (len(tn.Items) + 2) / 3; small.Pages != want {
		t.Errorf("%s: %s: the walk at page size 3 took %d page(s), want %d for %d items", label, tn.Name, small.Pages, want, len(tn.Items))
	}
	var order []string
	for _, it := range tn.Items {
		order = append(order, it.ID)
	}
	var gotOrder []string
	for _, row := range small.Rows {
		gotOrder = append(gotOrder, row.ItemID)
	}
	if strings.Join(order, ",") != strings.Join(gotOrder, ",") {
		t.Errorf("%s: %s: the walk at page size 3 returned the items in the order %v, want the order they were run: %v", label, tn.Name, gotOrder, order)
	}
	fullBy := full.byItem()
	for _, row := range small.Rows {
		f, ok := fullBy[row.ItemID]
		if !ok || f.State != row.State || f.Reason != row.Reason || f.ErrorClass != row.ErrorClass {
			t.Errorf("%s: %s: item %s reads (%s) at page size 3 and (%s) at page size 200", label, tn.Name, row.ItemID, row, f)
		}
	}
}

// evidence prints the sink's per-recipient counts and module 2's own job rows of the tenant: what the first-run review reads.
func (r *m2wRun) evidence(t *testing.T, label string, tn *m2wTenant) {
	t.Helper()
	r.noteSink(t, label, tn, r.sinkView(t))
	if rows, err := r.jobRows(r.ctx, tn); err == nil {
		t.Logf("NOTE module 2's job rows of %s (%s), read as the superuser:\n%s", tn.Name, label, rows)
	} else {
		t.Logf("NOTE could not read module 2's job rows of %s: %v", tn.Name, err)
	}
}

// touched is the recipients of the tenant the sink has answered at RCPT TO at least once (an SMTP session reached it), sorted.
func (r *m2wRun) touched(v m2wView, tn *m2wTenant) []string {
	var out []string
	for _, it := range tn.Items {
		if v.Rcpt[it.Email].tried() > 0 {
			out = append(out, it.Email)
		}
	}
	sort.Strings(out)
	return out
}

// checkRerun runs every item again: module 2's run is idempotent on the job item (200 and the job's CURRENT state, the same job id, no second job and so no second send). The sink's counts must not move.
func (r *m2wRun) checkRerun(t *testing.T, label string, tn *m2wTenant, final map[string]string, before m2wView) {
	t.Helper()
	for _, it := range tn.Items {
		status, jobID, state := r.runItem(t, tn, it)
		if status != http.StatusOK || jobID != it.JobID || state != final[it.ID] {
			t.Errorf("%s: %s item %02d: a second POST /v1/run answered HTTP %d, job %s, state %q; want 200, the same job %s and its current state %q", label, tn.Name, it.Index, status, jobID, state, it.JobID, final[it.ID])
		}
	}
	after := r.sinkView(t)
	for _, it := range tn.Items {
		if after.Rcpt[it.Email] != before.Rcpt[it.Email] || after.messages(it.Email) != before.messages(it.Email) {
			t.Errorf("%s: %s item %02d: the sink's counts moved after the second POST /v1/run (RCPT %+v -> %+v, messages %d -> %d): a re-run sent again", label, tn.Name, it.Index, before.Rcpt[it.Email], after.Rcpt[it.Email], before.messages(it.Email), after.messages(it.Email))
		}
	}
}

func m2wStates(rep m2wCampaign) map[string]string {
	out := map[string]string{}
	for _, row := range rep.Rows {
		out[row.ItemID] = row.State
	}
	return out
}

// checkAccountHealthy: sends, bounces and 4xx deferrals must leave the account usable. A bounce is the recipient's fault and does not touch the account; a transient 4xx without a rate signal requeues
// with a backoff and does not pause or quarantine it (module 2 classify.go, outcome.go). The quarantine and pause ladders are at their seeded `shadow`.
func (r *m2wRun) checkAccountHealthy(t *testing.T, label string, tn *m2wTenant) {
	t.Helper()
	a := r.account(t, tn)
	if a.Status.Paused || a.Status.Quarantined || a.Status.Disabled {
		t.Errorf("%s: %s: the account is paused=%v quarantined=%v disabled=%v after the scenario, want none of them", label, tn.Name, a.Status.Paused, a.Status.Quarantined, a.Status.Disabled)
	}
	t.Logf("NOTE account of %s after %s: paused=%v quarantined=%v disabled=%v daily_sent_count=%d (every attempt, failed or not, consumes a slot) account_status_score=%d", tn.Name, label, a.Status.Paused, a.Status.Quarantined, a.Status.Disabled, a.DailySentCount, a.Score)
}

// ---- the tests ----

// TestM2Worker_Preconditions proves what the other four tests stand on, each with its paired control: the operator surface is mounted and its key is not the platform's token (neither is accepted on the
// other's routes), the dials exist and are validated (an unknown one is 404, an out-of-range value is 400 and changes nothing), the worker is registered and heartbeating, and the sink is strict (a
// misspelt mode field is refused and changes nothing) and idle.
func TestM2Worker_Preconditions(t *testing.T) {
	r := m2wNewRun(t)
	r.prepare(t)

	// credentials: three different secrets, two domains, no cross-over. Each refusal is paired with the success of the right credential on the same route family.
	status, body := r.do(t, r.m2URL, http.MethodGet, "/v1/admin/health", r.token, nil)
	r.expect(t, "control: the PLATFORM token presented to the OPERATOR surface (the surface must be mounted and must refuse a credential that is not the operator key)", status, body, http.StatusUnauthorized, "unauthenticated")
	status, body = r.do(t, r.m2URL, http.MethodPost, "/v1/quote", r.adminKey, map[string]any{"recipient_count": 1})
	r.expect(t, "control: the OPERATOR key presented to the PLATFORM surface", status, body, http.StatusUnauthorized, "unauthenticated")
	status, body = r.do(t, r.m2URL, http.MethodPost, "/v1/quote", "", map[string]any{"recipient_count": 1})
	r.expect(t, "control: no credential on the platform surface", status, body, http.StatusUnauthorized, "unauthenticated")
	status, body = r.do(t, r.m2URL, http.MethodPost, "/v1/quote", r.token, map[string]any{"recipient_count": 3})
	r.expect(t, "the platform token on POST /v1/quote", status, body, http.StatusOK, "")
	h := r.health(t) // the operator key on the operator surface (r.health fails unless 200)
	t.Logf("NOTE module 2's health document: dispatch rung %q (paused %v), queue %v, boxes %+v, accounts %v", h.Dispatch.Rung, h.Dispatch.Paused, h.Queue, h.Boxes, h.Accounts)
	if h.Dispatch.Rung != "serving" || h.Dispatch.Paused {
		t.Errorf("module 2's dispatch rung is %q (paused %v): the scenarios need it serving", h.Dispatch.Rung, h.Dispatch.Paused)
	}

	// the dials: a name that does not exist is refused, a value out of range is refused and the stored value is unchanged
	status, body = r.admin(t, http.MethodPut, "/v1/admin/config/m2w_no_such_dial", map[string]any{"value": 1})
	r.expect(t, "control: PUT of a dial that does not exist (a misspelt dial name would read as a change that did nothing)", status, body, http.StatusNotFound, "not_found")
	status, body = r.admin(t, http.MethodPut, "/v1/admin/config/job_max_failed_attempts", map[string]any{"value": 0})
	r.expect(t, "control: PUT job_max_failed_attempts = 0 (the module's range is 1..1000)", status, body, http.StatusBadRequest, "validation_failed")
	status, body = r.admin(t, http.MethodGet, "/v1/admin/config/job_max_failed_attempts", nil)
	r.expect(t, "GET job_max_failed_attempts after the refused write", status, body, http.StatusOK, "")
	if !strings.Contains(strings.ReplaceAll(string(body), " ", ""), `"value":2`) {
		t.Errorf("job_max_failed_attempts is not 2 after a refused write of 0: %s", r.snip(body, 300))
	}

	// the worker (prepare asserted it; this adds the evidence a reviewer reads)
	var box string
	var beat float64
	if err := r.db.QueryRow(r.ctx, "SELECT status::text, EXTRACT(EPOCH FROM (now() - last_heartbeat_at))::float8 FROM send_managed.boxes WHERE id = $1::uuid", r.workerID).Scan(&box, &beat); err != nil {
		t.Fatalf("read the worker's box row: %v", err)
	}
	t.Logf("NOTE the worker %s is %s, last heartbeat %.1f s ago", r.workerID, box, beat)

	// the sink: strict (a misspelt field is a 400, not "no change") and in its default mode
	status, body = r.do(t, r.ctlURL, http.MethodPost, "/api/mode", "", map[string]any{"reject_rcpts": []string{"typo@example.test"}})
	if status != http.StatusBadRequest {
		t.Fatalf("control: the sink accepted a mode with a misspelt field (HTTP %d: %s): a test could set a hostile mode that does nothing and pass", status, r.snip(body, 300))
	}
	out := r.ctlCall(t, http.MethodGet, "/api/mode", nil, http.StatusOK)
	t.Logf("NOTE the sink's mode after the refused one: %s", strings.TrimSpace(string(out)))
	if v := r.inflight(t); v.Inflight != 0 {
		t.Errorf("the sink holds %d session(s) at the start", v.Inflight)
	}
}

// TestM2Worker_ControlAllSent is the paired control of the fault scenario: the SAME campaign (eight items, the same kinds of address, the same order) with the sink in its default mode, which accepts
// everything. Every item must be sent, exactly once. If this is red the worker or the SMTP dialogue is broken and the fault scenario proves nothing; if the fault scenario's bounced and exhausted items
// were also sent here, the sink's mode would be what made the difference, and it is.
func TestM2Worker_ControlAllSent(t *testing.T) {
	r := m2wNewRun(t)
	r.prepare(t)
	tn := r.newTenant(t, "ctl", []string{"ok", "bounce", "defer", "ok", "bounce", "defer", "ok", "ok"}, true)
	const label = "control (default sink)"

	q := r.quote(t, tn, len(tn.Items))
	t.Logf("NOTE quote for %d recipient(s): cost %d credits (unit %d, tier %s)", len(tn.Items), q.Cost, q.UnitPrice, q.Tier)
	took := r.runAll(t, tn)
	t.Logf("NOTE POST /v1/run: %d new jobs (202, queued) in %s", len(tn.Items), took.Round(time.Millisecond))
	rep := r.waitTerminal(t, tn, 90*time.Second, nil)

	r.checkPartition(t, label, tn, rep, "sent")
	r.checkTotals(t, label, tn, rep, m2wTotals{Total: 8, Sent: 8})
	r.checkOutcomes(t, label, tn, rep, func(*m2wItem) m2wWant { return m2wWantSent })
	view := r.sinkView(t)
	r.checkSink(t, label, tn, view, func(*m2wItem) m2wSinkWant { return m2wSinkWant{Messages: 1, Accepted: 1} })
	r.checkPaging(t, label, tn, rep)
	r.checkRerun(t, label, tn, m2wStates(rep), view)
	r.checkAccountHealthy(t, label, tn)
	r.evidence(t, label, tn)
}

// TestM2Worker_SentBouncedAndAttemptsExhausted is the fault scenario: ONE campaign of eight recipients that the sink is told to treat differently, on disjoint sets of addresses:
//
//	ok      (4)  accepted                                        -> sent
//	bounce  (2)  550 5.1.1 no such user at RCPT TO               -> bounced / bounce_permanent: tried ONCE, never retried, no message
//	defer   (2)  451 4.7.1 try later at RCPT TO                  -> requeued, retried, then failed / attempts_exhausted at the cap of TWO failed attempts: the sink answers 451 exactly twice
//
// With module 2's defaults the deferred items would need six attempts on a ladder of 30 s, 120 s and 600 s (about half an hour); the dials of m2wDials make it seconds. The assertions are the exact set
// equality of sent + bounced + failed with what was submitted, each recipient's final state, reason and error class in BOTH reports, and the sink's own counters as the independent witness.
func TestM2Worker_SentBouncedAndAttemptsExhausted(t *testing.T) {
	r := m2wNewRun(t)
	r.prepare(t)
	tn := r.newTenant(t, "fault", []string{"ok", "bounce", "defer", "ok", "bounce", "defer", "ok", "ok"}, true)
	const label = "fault"

	reject, deferred := tn.emails("bounce"), tn.emails("defer")
	if a, b := m2wDiff(reject, deferred); len(a) != len(reject) || len(b) != len(deferred) || len(reject) != 2 || len(deferred) != 2 || len(tn.emails("ok")) != 4 {
		t.Fatalf("the address sets are not disjoint and sized 4 accepted / 2 rejected / 2 deferred: reject %v defer %v", reject, deferred)
	}
	r.setMode(t, m2wMode{RejectRcpt: reject, RejectCode: 550, DeferRcpt: deferred})

	q := r.quote(t, tn, len(tn.Items))
	t.Logf("NOTE quote for %d recipient(s): cost %d credits (unit %d, tier %s)", len(tn.Items), q.Cost, q.UnitPrice, q.Tier)
	took := r.runAll(t, tn)
	t.Logf("NOTE POST /v1/run: %d new jobs (202, queued) in %s", len(tn.Items), took.Round(time.Millisecond))
	rep := r.waitTerminal(t, tn, 120*time.Second, nil)

	r.checkPartition(t, label, tn, rep, "sent", "bounced", "failed")
	r.checkTotals(t, label, tn, rep, m2wTotals{Total: 8, Sent: 4, Bounced: 2, Failed: 2})
	r.checkOutcomes(t, label, tn, rep, func(it *m2wItem) m2wWant {
		switch it.Kind {
		case "bounce":
			return m2wWantBounced
		case "defer":
			return m2wWantExhausted
		}
		return m2wWantSent
	})
	view := r.sinkView(t)
	r.checkSink(t, label, tn, view, func(it *m2wItem) m2wSinkWant {
		switch it.Kind {
		case "bounce":
			return m2wSinkWant{Rejected: 1} // tried once, refused once, never retried, never accepted, no message
		case "defer":
			return m2wSinkWant{Deferred: 2} // exactly the cap: attempt one, the requeue after the 1 s ladder step, attempt two, then failed
		}
		return m2wSinkWant{Messages: 1, Accepted: 1}
	})
	r.checkPaging(t, label, tn, rep)
	r.checkRerun(t, label, tn, m2wStates(rep), view)
	r.checkAccountHealthy(t, label, tn)
	r.evidence(t, label, tn)
}

// TestM2Worker_ControlHeldCampaignAllSent is the paired control of the cancel scenario: the SAME campaign (five items), the SAME sink mode (every send held for m2wHoldMs at the end of DATA), and NO
// cancel. Every item must be sent, one after the other (one worker, one lane), each held for about the hold, each exactly once. It proves that the items the cancel scenario ends as `cancelled` would
// have been delivered, and that the sink's hold is real (held_ms), so that "in flight" in the next test means what it says.
func TestM2Worker_ControlHeldCampaignAllSent(t *testing.T) {
	r := m2wNewRun(t)
	r.prepare(t)
	tn := r.newTenant(t, "hold", m2wHeldKinds(), true)
	const label = "control (held, not cancelled)"
	r.setMode(t, m2wMode{DataDelayMs: m2wHoldMs, DelayRecipients: tn.emails("")})

	r.quote(t, tn, len(tn.Items))
	r.runAll(t, tn)
	rep := r.waitTerminal(t, tn, time.Duration(len(tn.Items))*(m2wHoldMs*time.Millisecond+20*time.Second), nil)

	r.checkPartition(t, label, tn, rep, "sent")
	r.checkTotals(t, label, tn, rep, m2wTotals{Total: len(tn.Items), Sent: len(tn.Items)})
	r.checkOutcomes(t, label, tn, rep, func(*m2wItem) m2wWant { return m2wWantSent })
	view := r.sinkView(t)
	r.checkSink(t, label, tn, view, func(*m2wItem) m2wSinkWant { return m2wSinkWant{Messages: 1, Accepted: 1} })
	for _, it := range tn.Items {
		for _, m := range view.Msgs[it.Email] {
			if m.HeldMs < int64(m2wHoldMs)*9/10 {
				t.Errorf("%s: the sink held the final reply of %s for %d ms, want about %d: the sink's delay mode did not apply, so 'in flight' means nothing in the cancel test", label, it.Email, m.HeldMs, m2wHoldMs)
			}
		}
	}
	r.checkRerun(t, label, tn, m2wStates(rep), view)
	r.evidence(t, label, tn)
}

// TestM2Worker_CancelStopsSending is the cancel scenario, last because one worker takes one job at a time. A campaign of five items whose sends the sink holds for m2wHoldMs: the first item reaches
// DATA and is held (IN FLIGHT: the worker is waiting for the sink's reply), the other four are still queued. Then:
//
//	a bystander's cancel of the campaign           {0,0,0} and changes nothing (the tenant is the selector)
//	the owner's POST /v1/cancel                    a position, not a delta: cancelled + in_flight + already_terminal = 5
//	a second cancel straight after                 the same numbers (a no-op)
//	from the moment the cancel returned            NOTHING NEW reaches the sink: the set of recipients the sink has answered at RCPT TO does not grow, sampled at every poll and at the end
//	when the held send is released                 the in-flight item is reported exactly once, as sent (it finished and said so truthfully), with exactly one message at the sink; the other four are
//	                                               `cancelled` (reason cancelled), none of them has a message or an SMTP session
//	a third cancel after everything ended          the final position, no change; a re-run of every item answers 200, the same job, its final state, and sends nothing
//
// The paired control is TestM2Worker_ControlHeldCampaignAllSent: the same campaign and the same hold without the cancel ends with five messages at the sink.
func TestM2Worker_CancelStopsSending(t *testing.T) {
	r := m2wNewRun(t)
	r.prepare(t)
	tn := r.newTenant(t, "cancel", m2wHeldKinds(), true)
	bystander := r.newTenant(t, "bystander", nil, false)
	n := len(tn.Items)
	const label = "cancel"
	r.setMode(t, m2wMode{DataDelayMs: m2wHoldMs, DelayRecipients: tn.emails("")})

	r.quote(t, tn, n)
	r.runAll(t, tn)

	// one item reaches DATA and is held; the rest cannot start (one worker, one lane)
	held := r.waitInflight(t, 1, 60*time.Second)
	flying := map[string]bool{}
	for _, h := range held.Held {
		for _, a := range h.To {
			flying[strings.ToLower(a)] = true
		}
	}
	var flyingList []string
	for a := range flying {
		flyingList = append(flyingList, a)
	}
	sort.Strings(flyingList)
	known := r.known()
	for _, a := range flyingList {
		if it := known[a]; it == nil || !strings.HasPrefix(a, r.prefix()+tn.Name+"-") {
			t.Fatalf("the sink holds a send to %s: it is not an item of this campaign", a)
		}
	}
	if held.Held[0].RemainingMs < 3500 {
		t.Fatalf("the held send has only %d ms of its hold left: too late to cancel while it is in flight (the hold is %d ms; the runner is slow); nothing was asserted", held.Held[0].RemainingMs, m2wHoldMs)
	}
	pre := r.sinkView(t)
	if got := r.touched(pre, tn); strings.Join(got, ",") != strings.Join(flyingList, ",") {
		t.Errorf("%s: before the cancel the sink has answered RCPT TO for %v, want exactly the in-flight %v (one worker, one lane: nothing else may have started)", label, got, flyingList)
	}
	t.Logf("NOTE before the cancel: %d session(s) in flight %v (%d ms of the hold left), %d of %d items not started", held.Inflight, flyingList, held.Held[0].RemainingMs, n-len(flyingList), n)

	// a bystander (another user) cancelling the campaign: {0,0,0} and nothing changes
	c0 := r.cancel(t, bystander.UserID, tn.CampaignID)
	if c0.total() != 0 {
		t.Errorf("%s: a cancel of someone else's campaign answered (%s), want all zeros", label, c0)
	}
	if mid := r.campaign(t, tn.UserID, tn.CampaignID, 200); mid.Totals.Cancelled != 0 || mid.Totals.Total != n {
		t.Errorf("%s: after a bystander's cancel the campaign's totals are (%s), want nothing cancelled of %d", label, mid.Totals, n)
	}

	// the owner's cancel; the instant it returned is the start of the window in which nothing may reach the sink
	c1 := r.cancel(t, tn.UserID, tn.CampaignID)
	cancelledAt := time.Now()
	atCancel := r.sinkView(t)
	touchedAtCancel := r.touched(atCancel, tn)
	c2 := r.cancel(t, tn.UserID, tn.CampaignID)
	still := r.inflight(t)
	t.Logf("NOTE cancel #1 (the owner): %s; cancel #2 straight after: %s; the sink holds %d session(s) at that point; before the cancel returned the sink had answered RCPT TO for %v", c1, c2, still.Inflight, touchedAtCancel)
	if c1.total() != n {
		t.Errorf("%s: the first cancel's position adds up to %d (%s), want the %d items of the campaign", label, c1.total(), c1, n)
	}
	if c1.InFlight < 1 || c1.Cancelled < 1 {
		t.Errorf("%s: the first cancel answered (%s): want at least one item in flight (the held one) and at least one cancelled", label, c1)
	}
	if still.Inflight < 1 {
		t.Fatalf("the held send finished before the second cancel was made: the hold (%d ms) is too short for this runner; the second cancel's comparison with the first is not meaningful", m2wHoldMs)
	}
	if c2 != c1 {
		t.Errorf("%s: a second cancel straight after answered (%s), the first answered (%s): a second cancel must be a no-op with the same position", label, c2, c1)
	}
	if strings.Join(touchedAtCancel, ",") != strings.Join(flyingList, ",") {
		t.Errorf("%s: when the cancel returned the sink had answered RCPT TO for %v, want exactly the in-flight %v", label, touchedAtCancel, flyingList)
	}

	// the window: until every item is terminal, at every poll no recipient may have been tried that was not tried when the cancel returned
	seenNew := map[string]bool{}
	sample := func(m2wCampaign) {
		now := r.sinkView(t)
		for _, it := range tn.Items {
			if now.Rcpt[it.Email].tried() > 0 && !flying[it.Email] && !seenNew[it.Email] {
				seenNew[it.Email] = true
				t.Errorf("%s: %.1f s after the cancel returned the sink answered RCPT TO for %s: a send STARTED after the cancel (%+v)", label, time.Since(cancelledAt).Seconds(), it.Email, now.Rcpt[it.Email])
			}
		}
	}
	rep := r.waitTerminal(t, tn, m2wHoldMs*time.Millisecond+90*time.Second, sample)
	drain := time.Now()
	for r.inflight(t).Inflight > 0 {
		if time.Since(drain) > 30*time.Second {
			t.Fatalf("the sink still holds a session 30 s after the campaign was complete")
		}
		time.Sleep(300 * time.Millisecond)
	}
	time.Sleep(3 * time.Second) // a late send would show here; the window after the last report is part of "nothing new"
	end := r.sinkView(t)
	t.Logf("NOTE the window: from the cancel's return to %.1f s after it, no recipient other than %v was tried", time.Since(cancelledAt).Seconds(), flyingList)

	if got := r.touched(end, tn); strings.Join(got, ",") != strings.Join(flyingList, ",") {
		t.Errorf("%s: at the end the sink has answered RCPT TO for %v, want only the in-flight %v: something was sent after the cancel returned", label, got, flyingList)
	}
	r.checkPartition(t, label, tn, rep, "sent", "cancelled")
	r.checkTotals(t, label, tn, rep, m2wTotals{Total: n, Sent: len(flyingList), Cancelled: n - len(flyingList)})
	r.checkOutcomes(t, label, tn, rep, func(it *m2wItem) m2wWant {
		if flying[it.Email] {
			return m2wWantSent // in flight when the cancel came: it finishes and reports truthfully, exactly once
		}
		return m2wWantCancelled
	})
	r.checkSink(t, label, tn, end, func(it *m2wItem) m2wSinkWant {
		if flying[it.Email] {
			return m2wSinkWant{Messages: 1, Accepted: 1}
		}
		return m2wSinkWant{} // never started: no SMTP session, no message
	})
	r.checkPaging(t, label, tn, rep)

	// a third cancel, after everything ended: the final position, nothing changes
	c3 := r.cancel(t, tn.UserID, tn.CampaignID)
	if want := (m2wCancel{CampaignID: tn.CampaignID, Cancelled: n - len(flyingList), AlreadyTerminal: len(flyingList)}); c3 != want {
		t.Errorf("%s: a cancel after the campaign ended answered (%s), want the final position (%s)", label, c3, want)
	}
	r.checkRerun(t, label, tn, m2wStates(rep), end)
	if again := r.campaign(t, tn.UserID, tn.CampaignID, 200); again.Totals != rep.Totals {
		t.Errorf("%s: the campaign's totals changed after the third cancel and the re-run: (%s) -> (%s)", label, rep.Totals, again.Totals)
	}
	r.evidence(t, label, tn)
}

func m2wHeldKinds() []string {
	kinds := make([]string, m2wHeldItems)
	for i := range kinds {
		kinds[i] = "hold"
	}
	return kinds
}
