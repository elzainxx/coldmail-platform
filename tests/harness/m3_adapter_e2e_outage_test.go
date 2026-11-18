//go:build harness

package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/coldmail-platform/platform/internal/adapters/m3"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi/sendapitest"
	"github.com/coldmail-platform/platform/internal/modclient"
	"github.com/coldmail-platform/platform/internal/money"
)

// Phase 3, chunk B12 for module 3: the module goes down in the middle of a campaign and the platform neither invents nor loses anything. Module 3's Domain A API (the `m3` container: Control, the API the
// adapter talks to) is STOPPED by the stage script (tests/harness/m3adapter/outage.sh) after phase 1 and STARTED again after phase 2; module 3's three workers, its Postgres and the sink keep running. Three
// tests, three separate `go test` invocations (fresh test containers), in this order:
//
//	TestM3Adapter_OutagePhase1Start    a real tenant (login, platform user, entitlement delivered by the service, a verified account), a campaign of 24 whose every send the sink holds 6 s: proved still running
//	                                   by the adapter's own report AND by the sink (progress started, not complete, a send in flight, too much work left to finish before the stage stops the module); the state is recorded
//	TestM3Adapter_OutagePhase2Down     (the module is stopped) every read and write the platform makes fails TYPED (unavailable or timeout) and returns NO value; a polling loop stops by the context it is given; the platform's
//	                                   books are exactly what phase 1 recorded; module 3's own tables hold no job of the item Run was asked for; the platform's /readyz does not depend on module 3
//	TestM3Adapter_OutagePhase3Resume   (the module is started again, its workers too) polling recovers within a bounded budget and tells a transient from a wrong answer; the campaign completes EXACTLY ONCE: every item reported
//	                                   once, one job each (the ids phase 1 was given), one message at the sink each, one send-log row each, nothing charged, the books closed; a repeat Run of every item is the module's replay
//
// THE GUARD. The three tests do their real work only when HARNESS_M3_OUTAGE=1, which only the outage sub-stage sets (`docker compose run -e`). Without it each runs its paired controls and nothing else (they need no
// stack), so the ordinary run of stage 6, which lists and runs every test of the binary, counts them toward the floor harmlessly; the sub-stage requires, in the log of each invocation, both the `--- PASS` line AND the
// `NOTE m3a outage phaseN live:` line only a live run writes (m3a_require_phase), so a control-only PASS can never stand in for the real phase. What survives from one invocation to the next is a scratch table in the
// database the tests already reach as the superuser: harness_outage.m3 (key, value jsonb, recorded_at), created by phase 1, never part of a migration, and a phase that finds no row of the phase before it FAILS with a
// sentence (it never starts from nothing and never skips).
//
// THE CONTROL. The same flow with the module up is TestM3Adapter_EndToEndAccountsRunReportClassify (and the replay and exactly-once comparisons of phase 3 are the ones that test makes of its own campaign). Inside these
// tests every judge is shown to FAIL on a fake first: the judge of a call made while the module is down (no error, an error of another kind, a value beside the error), the comparison of the platform's books (a ledger row,
// a changed job item, a changed entitlement or delivery fact; but not the retry counter), the judge of "in flight" (a complete campaign, no progress, too little left), the judge of "polling halts" (a loop that ignores its
// context) and the judge of exactly-once (a duplicated message, a dropped item, an extra job, a replaced job id).
//
// WHAT "DOWN" MEANS HERE. `docker compose stop m3` is a graceful stop (SIGTERM; a 90 s grace period): the Control stops claiming and leaves anything else exactly as stored; the API and the worker link go away. The
// workers keep their SMTP sessions: a send in flight finishes at the sink and its outcome waits in the worker's buffer (box_outcome_buffer_ttl_sec 900) until the Control is back, when the worker replays it (module 3's
// Phase 7 design; the Control's boot rescan extends every in-flight lease by startup_reconnect_window_sec so the job is not reclaimed and sent twice). A worker with nothing buffered exits after about 30 s without a Control
// (restart: "no"); the stage plays the orchestrator and starts the three again.
//
// ASSERTED ONLY WHAT THE CONTRACT GUARANTEES. While the module is down the sink may still receive the messages of sends that were in flight: that is the module's own background work, so phase 2 notes the count and asserts
// only that no recipient has more than one message. What the platform guarantees while it is down, and what is asserted: nothing settled, nothing invented, nothing created, nothing lost.

const (
	m3aOutageEnv     = "HARNESS_M3_OUTAGE" // "1" only inside the outage sub-stage's invocations
	m3aOutageSchema  = "harness_outage"
	m3aOutageTable   = "m3"
	m3aOutageN       = 24   // the campaign: recipients the sink accepts, each held for m3aOutageHold
	m3aOutageHoldMs  = 6000 // far under module 3's 30 s smtp operation timeout, far over the stage's reaction time
	m3aOutageLanes   = 3    // the user's lanes: three sends in flight at once on the three workers
	m3aOutageMinDone = 3    // phase 1 samples the campaign once at least this many items are terminal
	m3aOutageMinWork = 30   // ... and at least this many seconds of work must be left (the lanes, the hold): more than the stage needs to stop the module
)

type m3aOutItem struct {
	Index int
	ID    string // public.job_items.id
	Email string
	JobID string // module 3's job id, as Run answered it; "" for the probe item
}

type m3aOutageSample struct {
	Total, Sent, InFlight, Pending, Bounced, Failed int64
	Complete                                        bool
	SinkHeld, SinkMessages                          int
	JobsByState                                     string
}

// m3aOutageState is what phase 1 hands to the next two phases.
type m3aOutageState struct {
	Run, Prefix, TenantName, UserID, CampaignID string
	AccountHost                                 string
	AccountIDs                                  []string
	Items                                       []m3aOutItem
	Probe                                       m3aOutItem // a job item that is committed and never handed to module 3 until phase 3: the write phase 2 must see fail and create nothing
	ProbeCampaign                               string
	HoldMs                                      int
	Books                                       m3aBooks
	Sample                                      m3aOutageSample
}

type m3aOutagePhase2 struct {
	Books        m3aBooks
	JobsByState  string
	Jobs         int
	SinkMessages int
	Kinds        map[string]string
	LoopRounds   int
}

// m3aOutageLive says whether this invocation is the outage sub-stage's. When it is not, it logs why the phase did nothing more than its controls and returns false.
func m3aOutageLive(t *testing.T, phase int) bool {
	t.Helper()
	if os.Getenv(m3aOutageEnv) == "1" {
		return true
	}
	t.Logf("NOTE m3a outage phase%d control-only: %s=1 is set only by the outage sub-stage (tests/harness/m3adapter/outage.sh), which runs the three phases against a stack it stops and starts between them; an ordinary run proves only that this phase's judges can fail (the controls above)", phase, m3aOutageEnv)
	return false
}

// ---- the judges, and the proof that each can fail ----

// m3aInFlightProblems judges the moment phase 1 samples the campaign: it is certainly still running (progress has started, it is not complete, a send is in flight at the sink) and what is left cannot finish
// before the stage has stopped the module (the remaining items, at the lanes and the hold of the sink, take at least minWorkSec seconds).
func m3aInFlightProblems(total int64, tt sendapi.Totals, complete bool, sinkHeld int, minDone int64, holdMs, lanes, minWorkSec int) []string {
	var p []string
	done := tt.Sent + tt.Bounced + tt.Failed
	if tt.Total != total {
		p = append(p, fmt.Sprintf("module 3 counts %d item(s) in the campaign, the platform handed it %d", tt.Total, total))
	}
	if complete {
		p = append(p, "the campaign is already complete: nothing is in flight to interrupt")
	}
	if done < minDone {
		p = append(p, fmt.Sprintf("only %d item(s) are terminal, want at least %d: no progress has started", done, minDone))
	}
	if sinkHeld < 1 {
		p = append(p, "the sink holds no send in flight")
	}
	if left := total - done; left > 0 && lanes > 0 {
		if sec := int((left+int64(lanes)-1)/int64(lanes)) * holdMs / 1000; sec < minWorkSec {
			p = append(p, fmt.Sprintf("%d item(s) are left, which at %d lane(s) and a %d ms hold is about %d s of work, want at least %d s: the campaign could end before the stage stops the module", left, lanes, holdMs, sec, minWorkSec))
		}
	} else if left <= 0 {
		p = append(p, "no item is left to send")
	}
	return p
}

// m3aLoopHalts runs a polling loop that must stop by the context it is given: it gives it a deadline and reports a loop that has not returned a grace period after it. rounds is what the loop reports.
func m3aLoopHalts(deadline, grace time.Duration, loop func(ctx context.Context) int) (int, []string) {
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	done := make(chan int, 1)
	start := time.Now()
	go func() { done <- loop(ctx) }()
	select {
	case rounds := <-done:
		var p []string
		if el := time.Since(start); el < deadline-100*time.Millisecond {
			p = append(p, fmt.Sprintf("the loop returned after %s, before its deadline of %s: it was not polling until its context ended", el.Round(time.Millisecond), deadline))
		}
		return rounds, p
	case <-time.After(deadline + grace):
		return 0, []string{fmt.Sprintf("the polling loop did not return within %s of its context's deadline: the platform could not halt it", grace)}
	}
}

// m3aOutageEndProblems judges the end of the campaign against what phase 1 recorded: every item once, one job each (the ids Run answered), one message each, one acceptance each, one send-log row each, nothing else.
func m3aOutageEndProblems(st m3aOutageState, items []sendapi.Item, jobs map[string]m3aJob, view m3aView, sendLogs, distinctLogs int) []string {
	var p []string
	ids := make([]string, len(st.Items))
	for i, it := range st.Items {
		ids[i] = it.ID
	}
	p = append(p, sendapitest.ExactSet(m3aItemIDs(items), ids)...)
	for _, it := range items {
		if it.State != "sent" || it.ErrorClass != nil || it.SMTPResponse != nil || it.ProviderMessageID == nil || it.Class != sendapi.ClassTerminal {
			p = append(p, fmt.Sprintf("item %s is reported %s / error class %s / provider text %s / message id present %v, want sent with no error class, no text and a message id", it.JobItemID, it.State, m3aText(it.ErrorClass), m3aText(it.SMTPResponse), it.ProviderMessageID != nil))
		}
	}
	if len(jobs) != len(st.Items) {
		p = append(p, fmt.Sprintf("module 3 holds %d job(s) of the tenant, want exactly %d (one per item handed over: nothing created by the outage, nothing duplicated)", len(jobs), len(st.Items)))
	}
	if _, ok := jobs[st.Probe.ID]; ok {
		p = append(p, "module 3 holds a job for the item that was never handed to it: a write made while it was down created something")
	}
	for _, it := range st.Items {
		j, ok := jobs[it.ID]
		switch {
		case !ok:
			p = append(p, fmt.Sprintf("module 3 holds no job for item %02d (%s)", it.Index, it.ID))
		case j.ID != it.JobID:
			p = append(p, fmt.Sprintf("the job of item %02d is %s, phase 1 was given %s: the job was replaced", it.Index, j.ID, it.JobID))
		case j.State != "sent":
			p = append(p, fmt.Sprintf("the job of item %02d is in state %s, want sent", it.Index, j.State))
		}
		if n := view.Counts[it.Email]; n != 1 {
			p = append(p, fmt.Sprintf("the sink holds %d message(s) to %s (item %02d), want exactly one: nothing may be sent twice or lost because of the outage", n, it.Email, it.Index))
		}
		if c := view.Rcpt[it.Email]; c.Accepted < 1 || c.Rejected != 0 || c.Deferred != 0 {
			p = append(p, fmt.Sprintf("the sink answered RCPT %+v for %s, want an acceptance and no rejection or deferral (an extra acceptance is a session cut before the final reply and is noted, not judged: the sink recorded no message for it)", c, it.Email))
		}
	}
	total := 0
	for _, it := range st.Items {
		total += view.Counts[it.Email]
	}
	if total != len(st.Items) {
		p = append(p, fmt.Sprintf("the sink holds %d message(s) to the campaign's recipients, want %d", total, len(st.Items)))
	}
	p = append(p, view.Dups...)
	if distinctLogs != len(st.Items) || sendLogs < len(st.Items) {
		p = append(p, fmt.Sprintf("module 3's send log holds %d row(s) of %d distinct job(s), want every one of the %d job(s) logged at least once (a job with no log row was never sent by a worker)", sendLogs, distinctLogs, len(st.Items)))
	}
	sort.Strings(p)
	return p
}

// m3aOutageControls shows every judge of the three phases to fail on a fake. Each returns the number of fakes caught; the test fails on a judge that accepts one.
func m3aOutageControls(t *testing.T, phase int) {
	t.Helper()
	caught := 0
	want := func(what string, p []string, reason string) {
		t.Helper()
		if len(p) == 0 || !strings.Contains(strings.Join(p, "\n"), reason) {
			t.Errorf("control: the judge of %s answered %v, want it to report %q", what, p, reason)
			return
		}
		caught++
	}
	wantNone := func(what string, p []string) {
		t.Helper()
		if len(p) != 0 {
			t.Errorf("control: the judge of %s reported %v on a good input: it rejects what it must accept", what, p)
		}
	}
	unavail := &modclient.Error{Kind: modclient.Unavailable, Module: modclient.M3, Route: "m3.report.campaign", Message: "connection refused"}
	timeout := &modclient.Error{Kind: modclient.Timeout, Module: modclient.M3, Route: "m3.report.campaign", Message: "the request timed out", Sent: true}
	refused := &modclient.Error{Kind: modclient.Refused, Module: modclient.M3, Route: "m3.run", Status: 409, Message: "refused", Sent: true}

	// the judge of a call made while the module is down (phase 2): it accepts the two right answers and reports each wrong one
	wantNone("a call while the module is down (unavailable, no value)", m3aDownProblems("a fake", unavail, true))
	wantNone("a call while the module is down (timeout, no value)", m3aDownProblems("a fake", timeout, true))
	want("a call while the module is down (no error, an answer)", m3aDownProblems("a fake", nil, false), "no error while the module is down")
	want("a call while the module is down (an error of another kind)", m3aDownProblems("a fake", refused, true), "is of kind refused")
	want("a call while the module is down (a value beside the error)", m3aDownProblems("a fake", unavail, false), "a value came back beside the error")
	want("a call while the module is down (an untyped error)", m3aDownProblems("a fake", errors.New("boom"), true), "is of kind unknown")
	if !m3aIsZero(sendapi.CampaignReport{}) || m3aIsZero(sendapi.CampaignReport{Pages: 1}) || !m3aIsZero([]m3.Account(nil)) || m3aIsZero([]m3.Account{{}}) || m3aIsZero(sendapi.WalkStats{Pages: 1}) {
		t.Errorf("control: m3aIsZero does not tell a zero value from a value")
	}

	// the comparison of the platform's books (phases 2 and 3)
	b := m3aBooks{Ledger: 1, JobItems: 25, JobItemsHash: "aa", Entitlement: "enabled", SyncFacts: "false/1/1/active/1/0"}
	wantNone("the platform's books (the retry counter alone moved)", m3aBooksDiff(b, m3aBooks{Ledger: 1, JobItems: 25, JobItemsHash: "aa", Entitlement: "enabled", SyncFacts: "false/1/1/active/1/0", Attempts: 5}))
	for _, c := range []struct {
		what, reason string
		mut          func(m3aBooks) m3aBooks
	}{
		{"a ledger row written during the outage", "the number of ledger rows changed", func(x m3aBooks) m3aBooks { x.Ledger++; return x }},
		{"a charge written during the outage", "monetary ledger rows changed", func(x m3aBooks) m3aBooks { x.Monetary++; return x }},
		{"a balance created during the outage", "balance rows changed", func(x m3aBooks) m3aBooks { x.Balances++; return x }},
		{"a job item row changed", "job item rows' fingerprint changed", func(x m3aBooks) m3aBooks { x.JobItemsHash = "bb"; return x }},
		{"the entitlement changed", "the entitlement changed", func(x m3aBooks) m3aBooks { x.Entitlement = "disabled"; return x }},
		{"a convergence recorded that module 3 was not seen to be in", "the delivery facts (platform.m3_sync) changed", func(x m3aBooks) m3aBooks { x.SyncFacts = "true/2/2/suspended/2/1"; return x }},
	} {
		want("the platform's books ("+c.what+")", m3aBooksDiff(b, c.mut(b)), c.reason)
	}
	want("the unmetered rule (a charge for a module 3 tenant)", m3aCompareBooks(m3aBooks{OtherLedger: 1, Monetary: 1}), "not entitlement rows")

	// the judge of "in flight" (phase 1)
	good := sendapi.Totals{Total: 24, Sent: 3, InFlight: 3, Pending: 18}
	wantNone("a campaign in flight", m3aInFlightProblems(24, good, false, 2, 3, 6000, 3, 30))
	want("a campaign in flight (already complete)", m3aInFlightProblems(24, sendapi.Totals{Total: 24, Sent: 24}, true, 0, 3, 6000, 3, 30), "already complete")
	want("a campaign in flight (no progress yet)", m3aInFlightProblems(24, sendapi.Totals{Total: 24, InFlight: 3, Pending: 21}, false, 3, 3, 6000, 3, 30), "no progress has started")
	want("a campaign in flight (nothing held at the sink)", m3aInFlightProblems(24, good, false, 0, 3, 6000, 3, 30), "the sink holds no send in flight")
	want("a campaign in flight (too little left)", m3aInFlightProblems(24, sendapi.Totals{Total: 24, Sent: 20, InFlight: 3, Pending: 1}, false, 1, 3, 6000, 3, 30), "could end before the stage stops the module")
	want("a campaign in flight (the module counts another number)", m3aInFlightProblems(24, sendapi.Totals{Total: 23, Sent: 3, Pending: 20}, false, 1, 3, 6000, 3, 30), "module 3 counts 23")

	// the judge of "polling halts by its context" (phase 2)
	rounds, p := m3aLoopHalts(300*time.Millisecond, 500*time.Millisecond, func(ctx context.Context) int {
		n := 0
		for {
			n++
			select {
			case <-ctx.Done():
				return n
			case <-time.After(40 * time.Millisecond):
			}
		}
	})
	if len(p) != 0 || rounds < 3 {
		t.Errorf("control: the judge of polling reported %v (%d rounds) on a loop that stops by its context", p, rounds)
	} else {
		caught++
	}
	var stop atomic.Bool
	_, p = m3aLoopHalts(150*time.Millisecond, 250*time.Millisecond, func(context.Context) int { // a loop that ignores its context
		for !stop.Load() {
			time.Sleep(10 * time.Millisecond)
		}
		return 0
	})
	stop.Store(true)
	if len(p) == 0 || !strings.Contains(strings.Join(p, "\n"), "could not halt it") {
		t.Errorf("control: the judge of polling accepted a loop that ignores its context: %v", p)
	} else {
		caught++
	}

	// the judge of exactly-once (phase 3)
	st := m3aOutageState{Probe: m3aOutItem{ID: "probe"}, Items: []m3aOutItem{{Index: 1, ID: "i1", Email: "a@x", JobID: "j1"}, {Index: 2, ID: "i2", Email: "b@x", JobID: "j2"}}}
	sentItem := func(id string) sendapi.Item {
		mid := "<m@x>"
		return sendapi.Item{JobItemID: id, State: "sent", Class: sendapi.ClassTerminal, ProviderMessageID: &mid}
	}
	items := []sendapi.Item{sentItem("i1"), sentItem("i2")}
	jobs := map[string]m3aJob{"i1": {ID: "j1", ItemID: "i1", State: "sent"}, "i2": {ID: "j2", ItemID: "i2", State: "sent"}}
	view := m3aView{Counts: map[string]int{"a@x": 1, "b@x": 1}, Rcpt: map[string]m3hRcpt{"a@x": {Accepted: 1}, "b@x": {Accepted: 1}}}
	copyView := func() m3aView {
		c := m3aView{Counts: map[string]int{}, Rcpt: map[string]m3hRcpt{}}
		for k, v := range view.Counts {
			c.Counts[k] = v
		}
		for k, v := range view.Rcpt {
			c.Rcpt[k] = v
		}
		return c
	}
	wantNone("the end of the campaign", m3aOutageEndProblems(st, items, jobs, view, 2, 2))
	{
		v := copyView()
		v.Counts["a@x"] = 2
		want("the end of the campaign (a message sent twice)", m3aOutageEndProblems(st, items, jobs, v, 2, 2), "want exactly one")
	}
	{
		v := copyView()
		v.Counts["b@x"] = 0
		want("the end of the campaign (a message lost)", m3aOutageEndProblems(st, items, jobs, v, 2, 2), "want exactly one")
	}
	{
		v := copyView()
		v.Rcpt["a@x"] = m3hRcpt{}
		want("the end of the campaign (a recipient the sink never accepted)", m3aOutageEndProblems(st, items, jobs, v, 2, 2), "want an acceptance and no rejection or deferral")
		v.Rcpt["a@x"] = m3hRcpt{Accepted: 1, Deferred: 1}
		want("the end of the campaign (a recipient the sink deferred)", m3aOutageEndProblems(st, items, jobs, v, 2, 2), "want an acceptance and no rejection or deferral")
		v.Rcpt["a@x"] = m3hRcpt{Accepted: 2}
		wantNone("the end of the campaign (an extra acceptance of a session that was cut before the final reply)", m3aOutageEndProblems(st, items, jobs, v, 2, 2))
	}
	want("the end of the campaign (an item dropped from the report)", m3aOutageEndProblems(st, items[:1], jobs, view, 2, 2), "missing i2")
	want("the end of the campaign (an item reported twice)", m3aOutageEndProblems(st, append(append([]sendapi.Item(nil), items...), items[0]), jobs, view, 2, 2), "appears 2 times")
	{
		j := map[string]m3aJob{"i1": jobs["i1"], "i2": jobs["i2"], "probe": {ID: "jp", ItemID: "probe", State: "queued"}}
		want("the end of the campaign (a job created by a write made while the module was down)", m3aOutageEndProblems(st, items, j, view, 2, 2), "never handed to it")
	}
	{
		j := map[string]m3aJob{"i1": {ID: "other", ItemID: "i1", State: "sent"}, "i2": jobs["i2"]}
		want("the end of the campaign (a job replaced)", m3aOutageEndProblems(st, items, j, view, 2, 2), "the job was replaced")
	}
	want("the end of the campaign (a job with no row in the send log)", m3aOutageEndProblems(st, items, jobs, view, 1, 1), "logged at least once")
	wantNone("the end of the campaign (a retried attempt: two rows for one job)", m3aOutageEndProblems(st, items, jobs, view, 3, 2))
	{
		it := append([]sendapi.Item(nil), items...)
		it[0].State = "queued"
		want("the end of the campaign (an item that is not sent)", m3aOutageEndProblems(st, it, jobs, view, 2, 2), "want sent")
	}
	t.Logf("NOTE m3a outage phase%d controls: %d fake inputs were each reported by the judge that must catch them (a call that returns a value or no error while the module is down, a ledger row, a charge, a balance, a changed job item, entitlement and delivery fact, an item in flight that is not, a loop that ignores its context, a duplicated or lost message, a recipient never accepted or deferred, a dropped, repeated or replaced item, a job created by a write, a job missing from the send log); the retry counter alone, an extra acceptance of a cut session and a retried send-log row are not changes", phase, caught)
}

// ---- the scratch state ----

func (e *m3aEnv) outageInit(t *testing.T) {
	t.Helper()
	for _, sql := range []string{
		"CREATE SCHEMA IF NOT EXISTS " + m3aOutageSchema,
		"DROP TABLE IF EXISTS " + m3aOutageSchema + "." + m3aOutageTable,
		"CREATE TABLE " + m3aOutageSchema + "." + m3aOutageTable + " (key text PRIMARY KEY, value jsonb NOT NULL, recorded_at timestamptz NOT NULL DEFAULT clock_timestamp())",
	} {
		if _, err := e.db.Exec(e.ctx, sql); err != nil {
			t.Fatalf("create the scratch state of the outage proof (%s): %v", sql, err)
		}
	}
}

func (e *m3aEnv) outageSave(t *testing.T, key string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encode the state %s: %v", key, err)
	}
	if _, err := e.db.Exec(e.ctx, "INSERT INTO "+m3aOutageSchema+"."+m3aOutageTable+" (key, value) VALUES ($1, $2::jsonb) ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, recorded_at = clock_timestamp()", key, string(b)); err != nil {
		t.Fatalf("record the state %s: %v", key, err)
	}
}

// outageLoad reads one row of the scratch state, and FAILS with a sentence when it is not there: a phase never starts from nothing and never skips.
func (e *m3aEnv) outageLoad(t *testing.T, key, producedBy string, dst any) time.Time {
	t.Helper()
	var exists bool
	if err := e.db.QueryRow(e.ctx, "SELECT to_regclass($1) IS NOT NULL", m3aOutageSchema+"."+m3aOutageTable).Scan(&exists); err != nil {
		t.Fatalf("look for the scratch state: %v", err)
	}
	if !exists {
		t.Fatalf("%s.%s does not exist: %s has not run in this stack (the three phases are run in order by the outage sub-stage, tests/harness/m3adapter/outage.sh); this phase has nothing to continue from and does not skip", m3aOutageSchema, m3aOutageTable, producedBy)
	}
	var raw string
	var at time.Time
	err := e.db.QueryRow(e.ctx, "SELECT value::text, recorded_at FROM "+m3aOutageSchema+"."+m3aOutageTable+" WHERE key = $1", key).Scan(&raw, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("the scratch state has no row %q: %s has not run (or failed before it recorded it); this phase has nothing to continue from and does not skip", key, producedBy)
	}
	if err != nil {
		t.Fatalf("read the state %s: %v", key, err)
	}
	if err := json.Unmarshal([]byte(raw), dst); err != nil {
		t.Fatalf("decode the state %s: %v", key, err)
	}
	return at.UTC()
}

// adopt makes this process act for the run phase 1 started: its prefix (the sink view's filter) and the sink's address the account names.
func (e *m3aEnv) adopt(st m3aOutageState) {
	e.runID, e.prefix, e.sinkIP = st.Run, st.Prefix, st.AccountHost
}

func (st m3aOutageState) tenant() *m3aTenant {
	tn := &m3aTenant{Name: st.TenantName, UserID: st.UserID, CampaignID: st.CampaignID, Prefix: fmt.Sprintf("%s%s-", st.Prefix, st.TenantName)}
	for _, it := range st.Items {
		tn.Items = append(tn.Items, &m3aItem{Index: it.Index, ID: it.ID, Email: it.Email, Kind: "ok", JobID: it.JobID})
	}
	for _, id := range st.AccountIDs {
		tn.Accounts = append(tn.Accounts, m3.Account{ID: id})
	}
	return tn
}

// seedExtraItem commits one more job item of the tenant in a campaign of its own (so it can be run while the first campaign is still active) and returns it.
func (e *m3aEnv) seedExtraItem(t *testing.T, tn *m3aTenant, label string) (m3aOutItem, string) {
	t.Helper()
	campaign := e.uuid(t)
	it := m3aOutItem{Email: fmt.Sprintf("%s%s@example.test", tn.Prefix, label)}
	err := e.db.QueryRow(e.ctx, `INSERT INTO public.job_items (campaign_id, user_id, recipient_email, rendered_subject, rendered_body)
		VALUES ($1::uuid, $2::uuid, $3, $4, $5) RETURNING id::text`, campaign, tn.UserID, it.Email, "[m3adapter "+e.runID+"] "+label, "<p>m3adapter "+e.runID+" "+label+"</p>").Scan(&it.ID)
	if err != nil {
		t.Fatalf("seed the %s job item: %v", label, err)
	}
	return it, campaign
}

func m3aPlatformURL() string {
	if v := strings.TrimRight(strings.TrimSpace(os.Getenv(m3aEnvPlatformURL)), "/"); v != "" {
		return v
	}
	return m3aDefaultPlatformURL
}

// ---- phase 1 ----

func TestM3Adapter_OutagePhase1Start(t *testing.T) {
	m3aOutageControls(t, 1)
	if !m3aOutageLive(t, 1) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), m3aBudget)
	t.Cleanup(cancel)
	e := newM3aEnv(t, ctx)
	e.prepare(t, true, false) // NO reset at the end: the hold at the sink must still be in force when the stage stops the module
	e.outageInit(t)

	kinds := make([]string, m3aOutageN)
	for i := range kinds {
		kinds[i] = "ok"
	}
	o := e.seedTenant(t, "out", kinds)
	probe, probeCampaign := e.seedExtraItem(t, o, "probe")
	e.setPacing(t, o, m3aOutageLanes, 0, 1)
	e.enable(t, o, "enable")
	st := e.waitSync(t, o, 1, false, "active", 60*time.Second)
	acct := e.createVerified(t, o, "1", e.sinkIP)
	e.setMode(t, m3hMode{DataDelayMs: m3aOutageHoldMs})
	took := e.runAll(t, o)
	t.Logf("NOTE m3a outage phase1 start: tenant %s (login, platform user and scope resolved through internal/tenant), entitlement delivered (%s), account %s verified at the sink; %d items run through the adapter in %s; the sink holds every send %d ms; %d lanes", o.UserID, m3aSyncString(st), acct.ID, len(o.Items), took.Round(time.Millisecond), m3aOutageHoldMs, m3aOutageLanes)

	// "still running", proved by the adapter's own report AND by the sink, with progress started
	var rep sendapi.CampaignReport
	var inf m3hInflight
	started := time.Now()
	m3lWait(t, ctx, fmt.Sprintf("at least %d item(s) of the campaign are terminal through the adapter, the campaign is not complete and the sink holds a send in flight", m3aOutageMinDone), 120*time.Second, 400*time.Millisecond, func() (bool, string) {
		r, err := o.C.ReportCampaign(ctx, o.CampaignID)
		if err != nil {
			t.Fatalf("ReportCampaign through the adapter failed (%s): %s", modclient.KindOf(err), e.raw.scrub(err.Error()))
		}
		rep, inf = r, e.inflight(t)
		e.requireNoDups(t, e.sinkView(t))
		done := r.Totals.Sent + r.Totals.Bounced + r.Totals.Failed
		return done >= m3aOutageMinDone && !r.Progress.IsComplete && inf.Inflight >= 1, fmt.Sprintf("%s; the sink holds %d send(s) in flight", m3aTotalsLine(r.Totals), inf.Inflight)
	})
	if p := m3aInFlightProblems(int64(len(o.Items)), rep.Totals, rep.Progress.IsComplete, inf.Inflight, m3aOutageMinDone, m3aOutageHoldMs, m3aOutageLanes, m3aOutageMinWork); len(p) != 0 {
		t.Fatalf("the campaign is not certainly in flight at the moment the stage will stop the module: %s", strings.Join(p, "; "))
	}
	jobs := e.jobsOf(t, o.UserID)
	if len(jobs) != len(o.Items) || jobs[probe.ID].ID != "" {
		t.Fatalf("module 3 holds %d job(s) of the tenant (%s), want exactly the %d items run and none for the probe item", len(jobs), e.jobStates(jobs), len(o.Items))
	}
	books := e.books(t, o.UserID)
	m3aProblems(t, "the tenant's books at the start", m3aCompareBooks(books))
	if books.Ledger != 1 || books.Entitlement != "enabled" || !strings.HasPrefix(books.SyncFacts, "false/1/1/active/1/") {
		t.Fatalf("the tenant's books at the start are %s, want one m3_enable ledger row, an enabled entitlement and a delivery converged at version 1 (active)", books)
	}
	e.requireBooks(t, "at the start of the outage proof")
	view := e.sinkView(t)

	state := m3aOutageState{Run: e.runID, Prefix: e.prefix, TenantName: o.Name, UserID: o.UserID, CampaignID: o.CampaignID, AccountHost: e.sinkIP, ProbeCampaign: probeCampaign, HoldMs: m3aOutageHoldMs, Probe: probe, Books: books,
		Sample: m3aOutageSample{Total: rep.Totals.Total, Sent: rep.Totals.Sent, InFlight: rep.Totals.InFlight, Pending: rep.Totals.Pending, Bounced: rep.Totals.Bounced, Failed: rep.Totals.Failed, Complete: rep.Progress.IsComplete, SinkHeld: inf.Inflight, SinkMessages: view.of(o), JobsByState: e.jobStates(jobs)}}
	for _, a := range o.Accounts {
		state.AccountIDs = append(state.AccountIDs, a.ID)
	}
	for _, it := range o.Items {
		state.Items = append(state.Items, m3aOutItem{Index: it.Index, ID: it.ID, Email: it.Email, JobID: it.JobID})
	}
	e.outageSave(t, "phase1", state)
	left := rep.Totals.Total - rep.Totals.Sent - rep.Totals.Bounced - rep.Totals.Failed
	t.Logf("NOTE m3a outage phase1 live: %d s after the run the adapter's report says %s, is_complete=%v; the sink holds %d send(s) in flight (%d held in all so far) and %d message(s) to the campaign; %d item(s) are not terminal, about %d s of work at %d lane(s) and a %d ms hold; module 3's own jobs: %s; the platform's books: %s; state recorded in %s.%s",
		int(time.Since(started).Seconds()), m3aTotalsLine(rep.Totals), rep.Progress.IsComplete, inf.Inflight, inf.HeldTotal, view.of(o), left, int((left+m3aOutageLanes-1)/m3aOutageLanes)*m3aOutageHoldMs/1000, m3aOutageLanes, m3aOutageHoldMs, e.jobStates(jobs), books, m3aOutageSchema, m3aOutageTable)
}

// ---- phase 2 ----

func TestM3Adapter_OutagePhase2Down(t *testing.T) {
	m3aOutageControls(t, 2)
	if !m3aOutageLive(t, 2) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), m3aBudget)
	t.Cleanup(cancel)
	e := newM3aEnv(t, ctx)
	var st m3aOutageState
	e.outageLoad(t, "phase1", "TestM3Adapter_OutagePhase1Start", &st)
	e.adopt(st)
	tn := st.tenant()
	tn.C = e.bind(t, tn, e.ad) // the default dials: a 20 s budget per attempt and two retries of a read
	// the short dials of a poller that must give up fast: one 3 s attempt and no retry. Both adapters are built exactly as phase 1's was (m3.FromConfig, the admin reader, the same tenant).
	fast := e.cfg
	fast.ModuleTimeout, fast.ModuleRetries = 3*time.Second, 0
	adFast := e.adapter(t, fast, e.adm, true)
	fc := e.bind(t, tn, adFast)

	// the module is really down, from the test container's own network
	u, err := url.Parse(e.raw.m3URL)
	if err != nil {
		t.Fatalf("module 3's URL: %v", err)
	}
	conn, derr := net.DialTimeout("tcp", u.Host, 3*time.Second)
	if derr == nil {
		_ = conn.Close()
		t.Fatalf("module 3's address %s still accepts a connection from the test container: the stage did not stop it, and an outage proof against a running module proves nothing", u.Host)
	}
	var dnsErr *net.DNSError
	how := "the connection was refused or timed out"
	if errors.As(derr, &dnsErr) {
		how = "the name no longer resolves (Docker removes a stopped container's name): modclient classifies a failed lookup as Unavailable"
	}
	t.Logf("NOTE m3a outage phase2 down: a TCP connection to module 3 from the test container fails: %s", how)

	first := tn.Items[0]
	var loopKinds []string
	kinds := map[string]string{}
	type result struct {
		name string
		err  error
		zero bool
	}
	callCtx := func() (context.Context, context.CancelFunc) { return context.WithTimeout(ctx, 15*time.Second) }
	var results []result
	do := func(name string, f func(ctx context.Context) (bool, error)) {
		c, cc := callCtx()
		defer cc()
		zero, err := f(c)
		results = append(results, result{name, err, zero})
	}
	do("ReportCampaign", func(c context.Context) (bool, error) {
		v, err := fc.ReportCampaign(c, st.CampaignID)
		return m3aIsZero(v), err
	})
	do("ReportCampaignPage", func(c context.Context) (bool, error) {
		v, err := fc.ReportCampaignPage(c, st.CampaignID, "")
		return m3aIsZero(v), err
	})
	do("Walk (the callback is never called)", func(c context.Context) (bool, error) {
		calls := 0
		stats, err := fc.Walk(c, st.CampaignID, func(sendapi.CampaignPage) error { calls++; return nil })
		return m3aIsZero(stats) && calls == 0, err
	})
	do("ReportJob", func(c context.Context) (bool, error) { v, err := fc.ReportJob(c, first.ID); return m3aIsZero(v), err })
	do("ReportJobByID", func(c context.Context) (bool, error) {
		v, err := fc.ReportJobByID(c, first.JobID)
		return m3aIsZero(v), err
	})
	do("Quote", func(c context.Context) (bool, error) {
		v, err := fc.Quote(c, int64(len(st.Items)))
		return m3aIsZero(v), err
	})
	do("Feasibility", func(c context.Context) (bool, error) {
		v, err := fc.Feasibility(c, int64(len(st.Items)))
		return m3aIsZero(v), err
	})
	var runErr, createErr error
	do("Run (of an item never handed to module 3)", func(c context.Context) (bool, error) {
		v, err := fc.Run(c, st.Probe.ID)
		runErr = err
		return m3aIsZero(v), err
	})
	do("ListAccounts", func(c context.Context) (bool, error) { v, err := fc.ListAccounts(c, ""); return m3aIsZero(v), err })
	do("AllAccounts", func(c context.Context) (bool, error) { v, err := fc.AllAccounts(c); return m3aIsZero(v), err })
	do("GetAccount", func(c context.Context) (bool, error) {
		v, err := fc.GetAccount(c, st.AccountIDs[0])
		return m3aIsZero(v), err
	})
	do("AccountEvents", func(c context.Context) (bool, error) {
		v, err := fc.AccountEvents(c, st.AccountIDs[0], "")
		return m3aIsZero(v), err
	})
	do("Suspension (the adapter's read through the admin reader)", func(c context.Context) (bool, error) {
		v, err := adFast.Suspension(c, st.UserID)
		return m3aIsZero(v), err
	})
	do("CreateAccount", func(c context.Context) (bool, error) {
		v, err := fc.CreateAccount(c, e.newAccount("down", tn, st.AccountHost))
		createErr = err
		return m3aIsZero(v), err
	})
	do("TestAccount", func(c context.Context) (bool, error) {
		v, err := fc.TestAccount(c, st.AccountIDs[0], false)
		return m3aIsZero(v), err
	})
	do("ReportCampaign with the default dials (20 s, two retries)", func(c context.Context) (bool, error) {
		v, err := tn.C.ReportCampaign(c, st.CampaignID)
		return m3aIsZero(v), err
	})
	for _, r := range results {
		m3aProblems(t, "phase 2", m3aDownProblems(r.name, r.err, r.zero))
		kinds[r.name] = m3aKindWord(r.err)
		loopKinds = append(loopKinds, fmt.Sprintf("%s -> %s", r.name, kinds[r.name]))
	}
	// a write that never left is not an orphan: a request refused before it reached the module (the name did not resolve, the connection was refused) says it was NOT sent, and a create that never reached the module
	// does not claim it may have been applied (only one that may have been is listed before the platform tries again)
	for _, w := range []struct {
		name string
		err  error
	}{{"Run", runErr}, {"CreateAccount", createErr}} {
		var re *modclient.Error
		if errors.As(w.err, &re) && re.Kind == modclient.Unavailable && re.Sent {
			t.Errorf("%s against an unavailable module says the request was sent (%s): a request that never left is not an orphan", w.name, re.Message)
		}
	}
	if modclient.KindOf(createErr) == modclient.Unavailable && m3.MaybeApplied(createErr) {
		t.Errorf("a CreateAccount that never reached module 3 says it may have been applied: after the restart the platform would list accounts for nothing")
	}
	t.Logf("NOTE m3a outage phase2 calls: %d calls made while the module is down, each typed unavailable or timeout with no value: %s", len(results), strings.Join(loopKinds, "; "))

	// polling that loops on the error stops by the context it was given
	var badKinds atomic.Int64
	rounds, problems := m3aLoopHalts(3*time.Second, 3*time.Second, func(c context.Context) int {
		n := 0
		for {
			n++
			_, err := fc.ReportCampaign(c, st.CampaignID)
			if k := modclient.KindOf(err); err == nil || (k != modclient.Unavailable && k != modclient.Timeout && k != modclient.Canceled) {
				badKinds.Add(1)
			}
			select {
			case <-c.Done():
				return n
			case <-time.After(250 * time.Millisecond):
			}
		}
	})
	m3aProblems(t, "the polling loop", problems)
	if rounds < 3 || badKinds.Load() != 0 {
		t.Errorf("the polling loop made %d round(s) with %d answer(s) that were neither unavailable, timeout nor canceled, want at least 3 rounds and no other kind", rounds, badKinds.Load())
	}
	t.Logf("NOTE m3a outage phase2 polling: a loop that polls the report and loops on the error made %d round(s) and stopped by its 3 s context", rounds)

	// the platform's own tables are exactly what phase 1 recorded: nothing settled, nothing invented
	after := e.books(t, st.UserID)
	m3aProblems(t, "the platform's books while module 3 is down", m3aBooksDiff(st.Books, after))
	m3aProblems(t, "the tenant's books while module 3 is down", m3aCompareBooks(after))
	e.requireBooks(t, "while module 3 is down")
	jobs := e.jobsOf(t, st.UserID)
	if len(jobs) != len(st.Items) || jobs[st.Probe.ID].ID != "" {
		t.Errorf("module 3's own tables hold %d job(s) of the tenant (%s), want the %d phase 1 ran and none for the item Run was asked for while the module was down: a write against a dead module must create nothing", len(jobs), e.jobStates(jobs), len(st.Items))
	}
	view := e.sinkView(t)
	if len(view.Dups) != 0 {
		t.Errorf("the sink shows a duplicate send while the module is down: %s", strings.Join(view.Dups, "; "))
	}
	tn2 := st.tenant()
	status, body := e.raw.do(t, m3aPlatformURL(), http.MethodGet, "/readyz", "", nil)
	if status != http.StatusOK {
		t.Errorf("the platform's /readyz answered HTTP %d while module 3 is down: %s; the platform's readiness is the database's and does not depend on a module (internal/httpapi/health.go)", status, e.raw.snip(body))
	}
	syncAfter, err := e.store.Sync(ctx, st.UserID)
	if err != nil {
		t.Fatalf("Store.Sync: %v", err)
	}
	if !syncAfter.Converged() || syncAfter.Observed != "active" || syncAfter.WantVersion != 1 {
		t.Errorf("the platform's delivery row is %s while module 3 is down, want it still converged at version 1 (active): the retry counter may rise, the facts never change", m3aSyncString(syncAfter))
	}
	e.outageSave(t, "phase2", m3aOutagePhase2{Books: after, JobsByState: e.jobStates(jobs), Jobs: len(jobs), SinkMessages: view.of(tn2), Kinds: kinds, LoopRounds: rounds})
	t.Logf("NOTE m3a outage phase2 live: the platform's books are unchanged from phase 1 (%s -> %s; retry counter %d -> %d is the platform retrying, by design); module 3's own tables still hold %d job(s) (%s) and none for the probe item; the sink holds %d message(s) to the campaign (%d at phase 1: the sends that were in flight finish at the sink while the Control is down) with no duplicate; the platform's /readyz is %d; platform.balance_drift and platform.reserve_overdrawn are empty",
		st.Books, after, st.Books.Attempts, after.Attempts, len(jobs), e.jobStates(jobs), view.of(tn2), st.Sample.SinkMessages, status)
}

// ---- phase 3 ----

func TestM3Adapter_OutagePhase3Resume(t *testing.T) {
	m3aOutageControls(t, 3)
	if !m3aOutageLive(t, 3) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), m3aBudget)
	t.Cleanup(cancel)
	e := newM3aEnv(t, ctx)
	e.prepare(t, false, true) // the three workers must be heartbeating again; the sink is NOT reset now (its messages and counters are the evidence of the outage), only when the test ends
	var st m3aOutageState
	var p2 m3aOutagePhase2
	e.outageLoad(t, "phase1", "TestM3Adapter_OutagePhase1Start", &st)
	e.outageLoad(t, "phase2", "TestM3Adapter_OutagePhase2Down", &p2)
	e.adopt(st)
	tn := st.tenant()
	tn.C = e.bind(t, tn, e.ad)
	t.Cleanup(func() {
		if t.Failed() {
			return
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		_, _ = e.db.Exec(ctx, "DROP TABLE IF EXISTS "+m3aOutageSchema+"."+m3aOutageTable)
	})

	// polling recovers: a typed Unavailable or Timeout while the module comes back is retried within a bounded budget; anything else is a wrong answer
	var rep sendapi.CampaignReport
	start := time.Now()
	var firstOK time.Time
	transient, consecutive := 0, 0
	last := ""
	for {
		r, err := tn.C.ReportCampaign(ctx, st.CampaignID)
		if err != nil {
			k := modclient.KindOf(err)
			recovering := firstOK.IsZero() && time.Since(start) < 90*time.Second
			flapping := !firstOK.IsZero() && consecutive < 3
			if (k == modclient.Unavailable || k == modclient.Timeout) && (recovering || flapping) {
				transient++
				consecutive++
				t.Logf("NOTE m3a outage phase3 t+%5.1fs: a transient %s while the module recovers (%d so far): %s", time.Since(start).Seconds(), k, transient, e.raw.scrub(err.Error()))
				time.Sleep(time.Second)
				continue
			}
			t.Fatalf("ReportCampaign after the restart failed with %s after %s (%d transient error(s) before): that is not a transient Unavailable or Timeout of a module coming back, it is a wrong answer: %s", k, time.Since(start).Round(time.Second), transient, e.raw.scrub(err.Error()))
		}
		consecutive = 0
		if firstOK.IsZero() {
			firstOK = time.Now()
			t.Logf("NOTE m3a outage phase3 t+%5.1fs: the module answers again after %d transient error(s)", time.Since(start).Seconds(), transient)
		}
		rep = r
		if line := m3aTotalsLine(r.Totals); line != last {
			last = line
			t.Logf("NOTE m3a outage phase3 t+%5.1fs: the adapter's campaign report (%d page(s), %d item(s)): %s", time.Since(start).Seconds(), r.Pages, len(r.Items), line)
		}
		e.requireNoDups(t, e.sinkView(t))
		if m3aAllTerminal(r, len(st.Items)) {
			break
		}
		if time.Since(start) > 300*time.Second {
			t.Fatalf("the campaign did not complete within 300 s of the restart: %s\n%s", m3aTotalsLine(r.Totals), e.diagnostics(t, tn))
		}
		select {
		case <-ctx.Done():
			t.Fatalf("the test's context ended while the campaign was completing: %v", ctx.Err())
		case <-time.After(m3aPollEvery):
		}
	}
	recovered := time.Since(start)
	time.Sleep(3 * time.Second) // a late duplicate would show up in this time
	view := e.sinkView(t)
	jobs := e.jobsOf(t, st.UserID)
	var sendLogs, distinct int
	if err := e.db.QueryRow(ctx, "SELECT count(*), count(DISTINCT job_id) FROM send_byo.send_logs WHERE user_id = $1::uuid", st.UserID).Scan(&sendLogs, &distinct); err != nil {
		t.Fatalf("count module 3's send log: %v", err)
	}

	// the campaign completed exactly once
	extraAccepted := 0
	for _, it := range st.Items {
		if c := view.Rcpt[it.Email]; c.Accepted > 1 {
			extraAccepted += c.Accepted - 1
		}
	}
	m3aProblems(t, "the campaign after the outage", m3aOutageEndProblems(st, rep.Items, jobs, view, sendLogs, distinct))
	m3aProblems(t, "the report against what was submitted", m3aCompareReport(tn, rep.Items))
	if rep.Totals.Total != int64(len(st.Items)) || rep.Totals.Sent != int64(len(st.Items)) || rep.Totals.Bounced != 0 || rep.Totals.Failed != 0 || rep.Totals.InFlight != 0 || rep.Totals.Pending != 0 || rep.Totals.WaitingForCapacity != 0 || !rep.ItemsMatchTotal() {
		t.Errorf("the final totals are (%s), want every one of the %d items sent and nothing else", m3aTotalsLine(rep.Totals), len(st.Items))
	}
	var sumSent int64
	for _, ba := range rep.ByAccount {
		sumSent += ba.Sent
	}
	if sumSent != rep.Totals.Sent {
		t.Errorf("totals_by_account adds up to %d sent, the totals say %d", sumSent, rep.Totals.Sent)
	}
	// every item through the single-job report too: the job each was given in phase 1
	for _, it := range tn.Items {
		one, err := tn.C.ReportJob(ctx, it.ID)
		if err != nil || one.JobID != it.JobID || one.State != "sent" {
			t.Errorf("the single-job report of item %02d is job %s state %s (error %v), want job %s sent", it.Index, one.JobID, one.State, err, it.JobID)
		}
	}
	cl, err := tn.C.Classify(rep.Items)
	if err != nil {
		t.Fatalf("Classify refused the report: %s", e.raw.scrub(err.Error()))
	}
	if len(cl.Settle) != len(st.Items) || len(cl.NotYet) != 0 || len(cl.Unmapped) != 0 {
		t.Errorf("Classify sorted the report into settle %d / not yet %v / unmapped %v, want all %d settled (free) and nothing else", len(cl.Settle), cl.NotYet, cl.Unmapped, len(st.Items))
	}
	m3aProblems(t, "the report, Classify and the charge table", m3aCompareClassified(rep.Items, cl, m3aDecide))

	// the books close: nothing charged, nothing reserved, nothing drifted, the delivery converged; a settle writes nothing (the money core refuses a module 3 job)
	books := e.books(t, st.UserID)
	m3aProblems(t, "the platform's books after the outage", m3aBooksDiff(st.Books, books))
	m3aProblems(t, "the tenant's books after the outage", m3aCompareBooks(books))
	e.requireBooks(t, "after the outage")
	var job string
	if err := e.db.QueryRow(ctx, "INSERT INTO platform.jobs (user_id, module_id) VALUES ($1::uuid, 'm3') RETURNING id::text", st.UserID).Scan(&job); err != nil {
		t.Fatalf("create the platform job: %v", err)
	}
	if _, err := e.money.Settle(ctx, money.SettleReq{JobID: job, Items: cl.Settle}); !errors.Is(err, money.ErrNotMetered) {
		t.Errorf("money.Settle of module 3's classified items answered %v, want ErrNotMetered", err)
	}
	if again := e.books(t, st.UserID); len(m3aBooksDiff(books, again)) != 0 {
		t.Errorf("the books changed after the refused settle: %v", m3aBooksDiff(books, again))
	}
	sync, err := e.store.Sync(ctx, st.UserID)
	if err != nil || !sync.Converged() || sync.Observed != "active" || sync.WantVersion != 1 {
		t.Errorf("the platform's delivery row is %s (error %v) after the restart, want it still converged at version 1 (active)", m3aSyncString(sync), err)
	}

	// a repeat Run of the same job item ids is the module's replay: 200, the job it already had, nothing sent
	replayed := 0
	for _, it := range tn.Items {
		res, err := tn.C.Run(ctx, it.ID)
		if err != nil || res.Created || res.JobID != it.JobID || res.State != "sent" || res.Class != sendapi.ClassTerminal {
			t.Errorf("a repeat Run of item %02d answered %+v (error %v), want the replay of job %s: 200, not created, sent, terminal", it.Index, res, err, it.JobID)
			continue
		}
		replayed++
	}
	time.Sleep(4 * time.Second)
	view2 := e.sinkView(t)
	jobs2 := e.jobsOf(t, st.UserID)
	if view2.of(tn) != view.of(tn) || len(view2.Msgs) != len(view.Msgs) || len(jobs2) != len(jobs) || len(view2.Dups) != 0 {
		t.Errorf("after the %d repeat Run calls the sink holds %d message(s) to the campaign (%d before) and module 3 %d job(s) (%d before): a replay sent again", replayed, view2.of(tn), view.of(tn), len(jobs2), len(jobs))
	}

	// the control of the write: the item Run was refused for while the module was down is accepted now that it is up, and creates exactly one job
	e.setMode(t, m3hMode{})
	probeRun, err := tn.C.Run(ctx, st.Probe.ID)
	if err != nil || !probeRun.Created || probeRun.State != "queued" {
		t.Errorf("control: the Run that failed while the module was down answered %+v (error %v) now it is up, want a new job (202, queued)", probeRun, err)
	} else {
		probeTn := &m3aTenant{Name: st.TenantName, UserID: st.UserID, CampaignID: st.ProbeCampaign, Prefix: tn.Prefix, C: tn.C, Accounts: tn.Accounts, Items: []*m3aItem{{Index: 1, ID: st.Probe.ID, Email: st.Probe.Email, Kind: "ok", JobID: probeRun.JobID}}}
		e.waitCampaign(t, probeTn, 90*time.Second, nil)
		if j := e.jobsOf(t, st.UserID); len(j) != len(st.Items)+1 || j[st.Probe.ID].ID != probeRun.JobID {
			t.Errorf("control: module 3 holds %d job(s) after the probe item was run, want %d (the campaign's and the one the write created now)", len(j), len(st.Items)+1)
		}
	}
	t.Logf("NOTE m3a outage phase3 live: %d transient error(s) while the module recovered; the campaign completed %s after the first poll: %s; every one of the %d items is reported once as sent, has the one job phase 1 was given, has ONE message at the sink and a row in module 3's send log (%d rows, %d distinct jobs; %d extra RCPT acceptance(s) at the sink: a session cut before its final reply, none expected); %d repeat Run calls were the module's replay and the sink stayed at %d messages; Classify settled %d items free by M3-01 and charged none; the platform's books are unchanged from phase 1 (%s); a settle of a module 3 job is ErrNotMetered; platform.balance_drift and platform.reserve_overdrawn are empty; the delivery row is converged at version 1; the item Run was refused for while the module was down is accepted now (control)",
		transient, recovered.Round(time.Second), m3aTotalsLine(rep.Totals), len(st.Items), sendLogs, distinct, extraAccepted, replayed, view2.of(tn), len(cl.Settle), books)
	t.Logf("NOTE m3a outage compare: phase 1 sampled %s (complete %v, %d held at the sink, %d message(s)); phase 2 saw the module's own jobs as %q and %d message(s) at the sink; phase 3 ended with %d message(s): the outage lost nothing and sent nothing twice", m3aSampleTotals(st.Sample), st.Sample.Complete, st.Sample.SinkHeld, st.Sample.SinkMessages, p2.JobsByState, p2.SinkMessages, view2.of(tn))
}

func m3aSampleTotals(s m3aOutageSample) string {
	return fmt.Sprintf("total=%d sent=%d bounced=%d failed=%d in_flight=%d pending=%d", s.Total, s.Sent, s.Bounced, s.Failed, s.InFlight, s.Pending)
}
