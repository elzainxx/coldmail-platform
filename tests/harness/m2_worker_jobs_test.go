//go:build harness && m2worker

package harness

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/adapters/m2"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/jobs"
)

// Phase 4, chunks B8 to B11 for module 2: the job engine of THIS process, over the production adapter built as internal/app builds it, against module 2's REAL Control, its REAL worker and the controllable SMTP
// sink (the stack of m2_worker_test.go: one worker, so one send at a time; one lane per user). The platform's job store runs on the live database as platform_app. What the module did is read three ways, none of
// them through the engine: module 2's own campaign report (the platform's adapter, built at the module's own address), the ledger and platform.job_results, and the sink's per-recipient counters.
//
// THE TENANT is m2wRun.newTenant without items (a real login, a platform user, a dedicated pool and one account naming the sink's private address); the rows are the job's own, with job_rows.id as the item id,
// and the tenant's campaign is the job's id (so that the failure cleanups of the m2worker stack cancel the right campaign). Every recipient carries this run's prefix so that the sink view is this run's.
// The module's dials are the m2wDials of the stack (two failed attempts end a job, a one-second retry ladder, no pause between sends, one lane per user, a 250 ms dispatch tick).

// jlM2Env is what the three tests share.
type jlM2Env struct {
	t      *testing.T
	r      *m2wRun
	direct *m2.Adapter // the platform's module 2 adapter at the module's OWN address: the test's independent reader of module 2's report
	capN   int         // module 2's job_max_failed_attempts: how many deferrals the sink sees of an item that ends failed/attempts_exhausted
}

func newJlM2Env(t *testing.T) *jlM2Env {
	t.Helper()
	r := m2wNewRun(t)
	r.prepare(t)
	direct, err := m2.FromConfig(config.Config{M2URL: r.m2URL, M2ServiceToken: r.token, ModuleTimeout: 20 * time.Second, ModuleRetries: 2, ModuleRetryBackoff: 250 * time.Millisecond})
	if err != nil {
		t.Fatalf("m2.FromConfig refused the harness configuration: %s", r.scrub(err.Error()))
	}
	m := &jlM2Env{t: t, r: r, direct: direct}
	status, body := r.admin(t, "GET", "/v1/admin/config/job_max_failed_attempts", nil)
	r.expect(t, "GET /v1/admin/config/job_max_failed_attempts", status, body, 200, "")
	var row struct {
		Value int64 `json:"value"`
	}
	if err := json.Unmarshal(body, &row); err != nil || row.Value < 1 {
		t.Fatalf("config job_max_failed_attempts is not a positive integer row (%v): %s", err, r.snip(body, 300))
	}
	m.capN = int(row.Value)
	return m
}

// stack builds a job stack whose module 2 adapter dials url (the module's own, or a forwarder's); a positive stale makes the engine raise a `stale` event after that long without progress.
func (m *jlM2Env) stack(url string, stale time.Duration) (*jlStack, *jlTrace) {
	m.t.Helper()
	o := jlOpts{Cfg: func(config.Secret) config.Config {
		return config.Config{M2URL: url, M2ServiceToken: m.r.token, ModuleTimeout: 20 * time.Second, ModuleRetries: 2, ModuleRetryBackoff: 250 * time.Millisecond}
	}}
	if stale > 0 {
		o.StaleAfter = map[string]time.Duration{jobs.ModuleSendManaged: stale}
	}
	return newJlStackWith(m.t, m.r.ctx, o)
}

// tenant makes a user with a pool and an account at module 2 and tells the stack's engine it is this test's.
func (m *jlM2Env) tenant(s *jlStack, name string) *m2wTenant {
	m.t.Helper()
	tn := m.r.newTenant(m.t, name, nil, true)
	s.adopt(tn.UserID)
	return tn
}

// recipients names one address per kind, in order (the form newTenant uses: <run prefix><tenant>-<kind>NN@example.test) and returns them with the kind of each.
func (m *jlM2Env) recipients(tn *m2wTenant, kinds []string) ([]string, map[string]string) {
	counts := map[string]int{}
	addrs := make([]string, len(kinds))
	kindOf := map[string]string{}
	for i, k := range kinds {
		counts[k]++
		addrs[i] = strings.ToLower(fmt.Sprintf("%s%s-%s%02d@example.test", m.r.prefix(), tn.Name, k, counts[k]))
		kindOf[addrs[i]] = k
	}
	return addrs, kindOf
}

func jlOfKind(addrs []string, kindOf map[string]string, kind string) []string {
	var out []string
	for _, a := range addrs {
		if kindOf[a] == kind {
			out = append(out, a)
		}
	}
	return out
}

// attach tells the run about the job's recipients (its pending rows): the campaign is the job's id and every pending row is an item of the tenant, so that the sink view and the cleanups know them.
func (m *jlM2Env) attach(tn *m2wTenant, job string, rows []jlRow, kindOf map[string]string) {
	m.t.Helper()
	tn.CampaignID = job
	tn.Items = nil
	for _, row := range rows {
		if row.State != "pending" {
			continue
		}
		it := &m2wItem{Index: len(tn.Items) + 1, ID: row.ID, Email: row.Norm, Kind: kindOf[row.Norm]}
		tn.Items = append(tn.Items, it)
		tn.byID[it.ID] = it
	}
}

// report is module 2's own report of the job's campaign, every page, read at the module's own address.
func (m *jlM2Env) report(user, job string) sendapi.CampaignReport {
	m.t.Helper()
	c, err := m.direct.For(user)
	if err != nil {
		m.t.Fatalf("m2.Adapter.For: %s", m.r.scrub(err.Error()))
	}
	rep, err := c.ReportCampaign(m.r.ctx, job)
	if err != nil {
		m.t.Fatalf("module 2's own report of the job failed: %s", m.r.scrub(err.Error()))
	}
	return rep
}

// view reads everything the three places say of the job: the platform's rows, results, ledger and price snapshot, module 2's report and the sink's counters of this tenant's recipients.
func (m *jlM2Env) view(s *jlStack, tn *m2wTenant, job string) jlM2View {
	m.t.Helper()
	rep := m.report(tn.UserID, job)
	sink := m.r.sinkView(m.t)
	mine := m.r.prefix() + tn.Name + "-"
	v := jlM2View{Rows: s.rows(job), Results: s.results(job), Ledger: s.ledgerCharges(job), Prices: s.prices(job), Report: rep.Items, Msgs: map[string]int{}, Rcpt: map[string]m2wRcpt{}, Cap: m.capN}
	for a, ms := range sink.Msgs {
		if strings.HasPrefix(a, mine) {
			v.Msgs[a] = len(ms)
		}
	}
	for a, c := range sink.Rcpt {
		if strings.HasPrefix(a, mine) {
			v.Rcpt[a] = c
		}
	}
	return v
}

// ---- the three-way comparison ------------------------------------------------------------------------------------------------------------------------------------------

// jlM2View is everything one module 2 job leaves behind.
type jlM2View struct {
	Rows    []jlRow
	Results []jlResult
	Ledger  []m1bCharge
	Prices  map[string]int // the job's frozen price snapshot
	Report  []sendapi.Item // module 2's own report
	Msgs    map[string]int // messages the sink holds, per recipient (this tenant's)
	Rcpt    map[string]m2wRcpt
	Cap     int // module 2's own cap of failed attempts: the deferrals the sink saw of an item that ended failed/attempts_exhausted
}

// jlM2Rule is the rule label of a module 2 result, from the table: M2-01 sent, M2-02 bounced, M2-03 failed for attempts_exhausted, M2-04 any other failure, M2-08 cancelled.
func jlM2Rule(state, reason string) string {
	switch state {
	case "sent":
		return "M2-01"
	case "bounced":
		return "M2-02"
	case "failed":
		if reason == "attempts_exhausted" {
			return "M2-03"
		}
		return "M2-04"
	case "cancelled":
		return "M2-08"
	}
	return "?"
}

// jlM2ThreeWay: for every row of the job ONE result; a pending row is in module 2's report exactly once and final, its result carries the report's state and reason, the decision and unit the hand-written oracle
// (m2eHand: sent and bounced are charged, a failed or cancelled item is free) makes of it and the rule the table labels it with, and the ledger holds ONE charge row of the oracle's unit at the frozen price for a
// charged item and none for a free one; the sink holds what module 2's state says (a sent item one message and one accepted RCPT, a bounced one one rejected RCPT and no message, an item that failed by attempts the
// module's cap of deferrals, a cancelled one nothing at all); a row of the platform's own (malformed, duplicate) has the platform's result, is free, is in no report and has no charge; and nothing in the results,
// the report, the ledger or the sink names a row or an address the job does not own.
func jlM2ThreeWay(v jlM2View) []string {
	var p []string
	say := func(f string, a ...any) { p = append(p, fmt.Sprintf(f, a...)) }
	res := jlByRef(v.Results)
	rep := map[string][]sendapi.Item{}
	for _, it := range v.Report {
		rep[it.JobItemID] = append(rep[it.JobItemID], it)
	}
	led := map[string][]m1bCharge{}
	for _, c := range v.Ledger {
		led[c.ItemRef] = append(led[c.ItemRef], c)
	}
	known := map[string]jlRow{}
	owner := map[string]int{}
	for _, row := range v.Rows {
		known[row.ID] = row
		if row.State == "pending" {
			owner[row.Norm]++
		}
	}
	for _, row := range v.Rows {
		rs := res[row.ID]
		if len(rs) != 1 {
			say("row %d (%s) has %d result(s), want exactly 1", row.Ordinal, row.State, len(rs))
			continue
		}
		r := rs[0]
		if row.State != "pending" {
			p = append(p, jlPlatformResultProblems(row, r)...)
			if len(rep[row.ID]) != 0 {
				say("module 2's report holds an item for row %d (%s): the platform ended it and never ran it", row.Ordinal, row.State)
			}
			if len(led[row.ID]) != 0 {
				say("the ledger holds a charge for row %d (%s): the platform's own outcomes are never charged", row.Ordinal, row.State)
			}
			continue
		}
		items := rep[row.ID]
		if len(items) != 1 {
			say("row %d is pending and module 2's report holds %d item(s) for it, want exactly 1", row.Ordinal, len(items))
			continue
		}
		it := items[0]
		reason := m2eReason(it.Reason)
		if it.Class != sendapi.ClassTerminal {
			say("row %d is reported in state %s (class %s), which is not final", row.Ordinal, it.State, it.Class)
		}
		if r.Origin != "module" || r.State != it.State || r.Reason != reason {
			say("row %d: the result says %s from %s (reason %q), module 2's report says %s (reason %q)", row.Ordinal, r.State, r.Origin, r.Reason, it.State, reason)
		}
		hand := m2eHand(it.State)
		if hand.Kind != "charge" && hand.Kind != "free" {
			say("row %d is in state %q, which a finished job cannot hold", row.Ordinal, it.State)
			continue
		}
		if r.Decision != hand.Kind {
			say("row %d: the result says %q, the hand-written oracle says %q for a %s item", row.Ordinal, r.Decision, hand.Kind, it.State)
		}
		wantUnit := ""
		if hand.Kind == "charge" {
			wantUnit = hand.Unit
		}
		if r.Unit != wantUnit {
			say("row %d: the result names unit %q, the oracle says %q", row.Ordinal, r.Unit, wantUnit)
		}
		if want := jlM2Rule(it.State, reason); r.Rule != want {
			say("row %d: the result is under rule %q, the table says %q for a %s item", row.Ordinal, r.Rule, want, it.State)
		}
		cs := led[row.ID]
		switch {
		case hand.Kind != "charge" && len(cs) != 0:
			say("row %d is a free %s item and the ledger holds %d charge row(s) for it", row.Ordinal, it.State, len(cs))
		case hand.Kind == "charge" && len(cs) != 1:
			say("row %d is a %s item: the ledger holds %d charge row(s) for it, want exactly 1", row.Ordinal, it.State, len(cs))
		case hand.Kind == "charge":
			if cs[0].Unit != hand.Unit {
				say("row %d is charged as unit %s, the table says %s", row.Ordinal, cs[0].Unit, hand.Unit)
			}
			if want, ok := v.Prices[hand.Unit]; !ok || cs[0].Cost != want || cs[0].Units != 1 {
				say("row %d is charged %d x %d credit(s), the frozen price of %s is %d for one unit", row.Ordinal, cs[0].Units, cs[0].Cost, hand.Unit, v.Prices[hand.Unit])
			}
		}
		w, ok := m2eSinkWantFor(it.State, v.Cap)
		c, n := v.Rcpt[row.Norm], v.Msgs[row.Norm]
		switch {
		case !ok:
			say("row %d is in state %q, which has no sink expectation", row.Ordinal, it.State)
		case n != w.Messages || c.Accepted != w.Accepted || c.Rejected != w.Rejected || c.Deferred != w.Deferred:
			say("row %d is %s: the sink holds %d message(s) and answered RCPT accepted %d rejected %d deferred %d, want %d, %d, %d, %d", row.Ordinal, it.State, n, c.Accepted, c.Rejected, c.Deferred, w.Messages, w.Accepted, w.Rejected, w.Deferred)
		}
	}
	for ref, rs := range res {
		if _, ok := known[ref]; !ok {
			say("%d result(s) belong to a row the job does not have", len(rs))
		}
	}
	for id := range rep {
		if _, ok := known[id]; !ok {
			say("module 2's report holds an item that is no row of the job")
		}
	}
	for ref := range led {
		if _, ok := known[ref]; !ok {
			say("the ledger holds a charge for an item that is no row of the job")
		}
	}
	for a, n := range v.Msgs {
		if owner[a] == 0 && n != 0 {
			say("the sink holds %d message(s) to an address that no pending row of the job owns", n)
		}
	}
	for a, c := range v.Rcpt {
		if owner[a] == 0 && c != (m2wRcpt{}) {
			say("the sink answered RCPT %+v for an address that no pending row of the job owns", c)
		}
	}
	for _, n := range owner {
		if n > 1 {
			say("two pending rows own one address: the platform's duplicate rule did not hold")
		}
	}
	sort.Strings(p)
	return p
}

func (v jlM2View) clone() jlM2View {
	w := v
	w.Rows = append([]jlRow(nil), v.Rows...)
	w.Results = append([]jlResult(nil), v.Results...)
	w.Ledger = append([]m1bCharge(nil), v.Ledger...)
	w.Report = append([]sendapi.Item(nil), v.Report...)
	w.Msgs, w.Rcpt = map[string]int{}, map[string]m2wRcpt{}
	for k, x := range v.Msgs {
		w.Msgs[k] = x
	}
	for k, x := range v.Rcpt {
		w.Rcpt[k] = x
	}
	return w
}

// jlM2Controls tampers with copies of an accepted view, one way at a time, and requires jlM2ThreeWay to say so: the comparison is the instrument of every module 2 job test here, and one that cannot fail proves nothing.
func jlM2Controls(t *testing.T, v jlM2View) {
	t.Helper()
	if p := jlM2ThreeWay(v); len(p) != 0 {
		t.Fatalf("control setup: the accepted view disagrees with itself: %v", p)
	}
	itemOf := map[string]sendapi.Item{}
	for _, it := range v.Report {
		itemOf[it.JobItemID] = it
	}
	var sent, platform jlRow
	for _, row := range v.Rows {
		switch it, in := itemOf[row.ID]; {
		case row.State == "pending" && in && it.State == "sent" && sent.ID == "":
			sent = row
		case row.State != "pending" && platform.ID == "":
			platform = row
		}
	}
	if sent.ID == "" {
		t.Fatal("control setup: the job needs a sent row")
	}
	type tamper struct {
		name, mention string
		tamper        func(w *jlM2View)
	}
	var controls []tamper
	if platform.ID != "" { // a job whose list holds no row of the platform's own has nothing to claim
		controls = append(controls, tamper{"a result of the platform's own claimed by the module", "want platform", func(w *jlM2View) {
			for i := range w.Results {
				if w.Results[i].Ref == platform.ID {
					w.Results[i].Origin = "module"
				}
			}
		}})
	}
	for _, c := range append(controls, []tamper{
		{"a result's state changed", "module 2's report says", func(w *jlM2View) {
			for i := range w.Results {
				if w.Results[i].Ref == sent.ID {
					w.Results[i].State = "failed"
				}
			}
		}},
		{"a charge dropped from the ledger", "the ledger holds 0 charge row(s) for it, want exactly 1", func(w *jlM2View) {
			for i, c := range w.Ledger {
				if c.ItemRef == sent.ID {
					w.Ledger = append(w.Ledger[:i:i], w.Ledger[i+1:]...)
					return
				}
			}
		}},
		{"a charge for an item that is no row of the job", "the ledger holds a charge for an item that is no row of the job", func(w *jlM2View) {
			w.Ledger = append(w.Ledger, m1bCharge{ItemRef: jlID(999), Unit: "m2_accepted", Cost: 1, Units: 1})
		}},
		{"a charge in the wrong unit", "is charged as unit", func(w *jlM2View) {
			for i := range w.Ledger {
				if w.Ledger[i].ItemRef == sent.ID {
					w.Ledger[i].Unit = "m2_bounced"
				}
			}
		}},
		{"a charge at the wrong price", "the frozen price of", func(w *jlM2View) {
			for i := range w.Ledger {
				if w.Ledger[i].ItemRef == sent.ID {
					w.Ledger[i].Cost++
				}
			}
		}},
		{"a second message to a sent recipient", "the sink holds 2 message(s)", func(w *jlM2View) { w.Msgs[sent.Norm] = 2 }},
		{"a sent item the sink never saw", "the sink holds 0 message(s)", func(w *jlM2View) { w.Msgs[sent.Norm] = 0 }},
		{"an item of module 2's report dropped", "module 2's report holds 0 item(s) for it", func(w *jlM2View) {
			for i, it := range w.Report {
				if it.JobItemID == sent.ID {
					w.Report = append(w.Report[:i:i], w.Report[i+1:]...)
					return
				}
			}
		}},
		{"a message to an address no pending row owns", "no pending row of the job owns", func(w *jlM2View) { w.Msgs["m2w-nobody-owns-this@example.test"] = 1 }},
	}...) {
		w := v.clone()
		c.tamper(&w)
		jlWantProblem(t, "control: "+c.name, c.mention, jlM2ThreeWay(w))
	}
}

// ---- the money of a finished job ---------------------------------------------------------------------------------------------------------------------------------------------

// jlM2Totals counts the final items of a module 2 report by state.
func jlM2Totals(items []sendapi.Item) (sent, bounced, failed, cancelled int) {
	for _, it := range items {
		switch it.State {
		case "sent":
			sent++
		case "bounced":
			bounced++
		case "failed":
			failed++
		case "cancelled":
			cancelled++
		}
	}
	return
}

// requireBooksOfJob asserts the money of a finished module 2 job: the reserve held units x the dearest reachable unit's price (and the preview said so), it was charged exactly what the report implies at the frozen
// prices, the remainder was released ONCE (one release row for it, none when nothing was left), the balance is the grant less what was charged and nothing is reserved, the ledger's charge rows are the view's, and the
// books' three proofs are empty. It returns the credits charged.
func (m *jlM2Env) requireBooksOfJob(s *jlStack, user, job string, pv jobs.Preview, units int, grant int64, v jlM2View) int64 {
	t := m.t
	t.Helper()
	var dearest int64
	for _, c := range v.Prices {
		dearest = max(dearest, int64(c))
	}
	if dearest < 1 || int64(pv.UnitCost) != dearest || pv.Credits != int64(units)*dearest || pv.Units != units {
		t.Fatalf("the preview says %d unit(s) at %d credits (%s) = %d, the job's frozen prices are %v: want the reserve at the DEAREST reachable unit (%d) over %d unit(s)", pv.Units, pv.UnitCost, pv.RateUnit, pv.Credits, v.Prices, dearest, units)
	}
	var want int64
	for _, it := range v.Report {
		switch it.State {
		case "sent":
			want += int64(v.Prices["m2_accepted"])
		case "bounced":
			want += int64(v.Prices["m2_bounced"])
		}
	}
	reserved, charged, released, ok := s.reserveOf(job)
	left := pv.Credits - want
	if !ok || reserved != pv.Credits || charged != want || released != left || reserved-charged-released != 0 {
		t.Fatalf("the reserve is (reserved %d, charged %d, released %d, exists %v), the report implies charged %d of the %d held (remainder %d): want the whole reserve accounted for, remainder 0", reserved, charged, released, ok, want, pv.Credits, left)
	}
	wantRows := int64(0)
	if left > 0 {
		wantRows = 1
	}
	if rows, credits := s.releaseRows(job); rows != wantRows || credits != left {
		t.Fatalf("the job has %d release row(s) for %d credits, want %d row(s) for %d (the remainder, once; a remainder of 0 writes no row)", rows, credits, wantRows, left)
	}
	if av, rv := s.balance(user, jobs.ModuleSendManaged); av != grant-want || rv != 0 {
		t.Fatalf("at the end the balance is available %d reserved %d, want %d and 0 (the grant less what was charged)", av, rv, grant-want)
	}
	if got := s.chargeRows(job); got != int64(len(v.Ledger)) {
		t.Fatalf("the job's charge rows read %d, the view holds %d", got, len(v.Ledger))
	}
	s.requireBooksScoped("after the module 2 job", job)
	return want
}

// requireNoModuleJobsFor asserts the platform's own classes never reached module 2: it holds exactly the pending rows' jobs for the campaign.
func (m *jlM2Env) requireModuleJobs(job string, want int) {
	m.t.Helper()
	var n int
	if err := m.r.db.QueryRow(m.r.ctx, `SELECT count(*) FROM send_managed.jobs WHERE campaign_id = $1::uuid`, job).Scan(&n); err != nil {
		m.t.Fatalf("count module 2's own jobs of the campaign: %v", err)
	}
	if n != want {
		m.t.Fatalf("module 2 holds %d job(s) of the campaign, want %d (one per pending row, none for a malformed or repeated row: the platform's classes never reach the module)", n, want)
	}
}

// ---- send, item by item -----------------------------------------------------------------------------------------------------------------------------------------------------

// TestJobsLive_M2_SendJobSettlesItemByItem (Phase 4, chunks B8 and B9, with the REAL module 2, its worker and the controllable sink): a user with 1000 credits sends a job of FIFTEEN rows: twelve good recipients (eight
// the sink accepts, two it rejects with 550 at RCPT, two it defers with 451 until module 2's own cap of attempts ends them), two addresses the platform's rule refuses and one repeat of the first recipient in
// capitals. The engine of this process writes public.job_items (EnsureModuleItems), runs each pending row at module 2 (Submit), reads the campaign report (Observe) and settles it: so twelve rows reach module 2 and
// three are the platform's own (P-08 x2, P-07).
//
// THE PROPERTIES, each from the three places (jlM2ThreeWay): the report (eight sent, two bounced, two failed by attempts_exhausted), the results (the report's states and reasons; the decision and unit the hand
// oracle gives: sent charged m2_accepted, bounced m2_bounced, failed free by M2-03; the platform's three free by P-08 and P-07), the LEDGER (one charge row per sent and per bounced item at the frozen price, none for a
// free one) and the SINK (one message and one accepted RCPT per sent recipient, exactly ONE for the first recipient although the list holds it twice, one rejected RCPT and no message per bounced one, the cap's
// deferrals per exhausted one, nothing for a row the platform ended). And: the reserve was held at the dearest reachable unit over the twelve units the preview said, charged what the report implies, the remainder
// released ONCE; module 2 holds twelve jobs of the campaign and none for the platform's classes; the job moved to confirmed, running, completing and succeeded once each; no flag, no stale event.
//
// THE CONTROLS: the same comparison on tampered copies of the view it just accepted (a result's state, a charge dropped, invented, in the wrong unit or at the wrong price, a second message, a message that never
// was, an item dropped from the report, a platform result claimed by the module, an address nobody owns) must report each, for its own reason.
func TestJobsLive_M2_SendJobSettlesItemByItem(t *testing.T) {
	m := newJlM2Env(t)
	s, _ := m.stack(m.r.m2URL, 0)
	tn := m.tenant(s, "jlsend")
	const grant = 1000
	s.grant(tn.UserID, jobs.ModuleSendManaged, grant)

	kinds := []string{"ok", "ok", "bounce", "ok", "defer", "ok", "ok", "bounce", "ok", "defer", "ok", "ok"}
	addrs, kindOf := m.recipients(tn, kinds)
	const units = 12
	list := jlBuildList(addrs, 2, 1)
	m.r.setMode(t, m2wMode{RejectRcpt: jlOfKind(addrs, kindOf, "bounce"), RejectCode: 550, DeferRcpt: jlOfKind(addrs, kindOf, "defer")})
	diag := func() string { return m.r.diagnostics(t, tn) }

	j := s.create(tn.UserID, jobs.CreateReq{Module: jobs.ModuleSendManaged, Rows: list.Rows, Content: jlContent("m2 send settles item by item")})
	pv := s.preview(tn.UserID, j.ID)
	if pv.Rows != units+3 || pv.Pending != units || pv.Malformed != 2 || pv.Duplicates != 1 || pv.Units != units || !pv.Metered || pv.UnitCost < 1 || pv.Credits != int64(units)*int64(pv.UnitCost) || pv.Available != grant || pv.Shortfall != 0 {
		t.Fatalf("the preview counts rows %d pending %d malformed %d duplicates %d units %d metered %v credits %d (rate %d, %s) available %d shortfall %d, want %d, %d, 2, 1, %d, metered, units x rate, the grant of %d and no shortfall",
			pv.Rows, pv.Pending, pv.Malformed, pv.Duplicates, pv.Units, pv.Metered, pv.Credits, pv.UnitCost, pv.RateUnit, pv.Available, pv.Shortfall, units+3, units, units, grant)
	}
	res := s.confirmOK(tn.UserID, j.ID, pv.Fingerprint)
	if res.Job.Status != jobs.StatusConfirmed || res.Reserved != pv.Credits || res.Pending != units {
		t.Fatalf("the confirm answered status %s reserved %d pending %d, want confirmed, %d and %d", res.Job.Status, res.Reserved, res.Pending, pv.Credits, units)
	}
	if av, rv := s.balance(tn.UserID, jobs.ModuleSendManaged); av != grant-pv.Credits || rv != pv.Credits {
		t.Fatalf("after the confirm the balance is available %d reserved %d, want %d and %d", av, rv, grant-pv.Credits, pv.Credits)
	}
	m.attach(tn, j.ID, s.rows(j.ID), kindOf)
	if n := s.count("count the job items of the campaign", `SELECT count(*) FROM public.job_items WHERE campaign_id = $1::uuid`, j.ID); n != 0 {
		t.Fatalf("public.job_items holds %d row(s) of the campaign before the engine ran: the confirm must not write the module's rows", n)
	}
	m.requireModuleJobs(j.ID, 0)

	s.tickToStatus(j.ID, jobs.StatusSucceeded, 5*time.Minute, diag)

	v := m.view(s, tn, j.ID)
	if problems := jlM2ThreeWay(v); len(problems) != 0 {
		t.Fatalf("module 2's report, the results, the ledger and the sink disagree (%d):\n  %s", len(problems), strings.Join(problems, "\n  "))
	}
	nSent, nBounced, nFailed, nCancelled := jlM2Totals(v.Report)
	if nSent != 8 || nBounced != 2 || nFailed != 2 || nCancelled != 0 || len(v.Report) != units {
		t.Fatalf("module 2 reports %d sent, %d bounced, %d failed and %d cancelled of %d item(s), want 8, 2, 2, 0 of %d (the scenario drives each outcome)", nSent, nBounced, nFailed, nCancelled, len(v.Report), units)
	}
	for _, it := range v.Report {
		if it.State == "failed" && m2eReason(it.Reason) != "attempts_exhausted" {
			t.Errorf("a failed item is reported with reason %q, want attempts_exhausted (the sink deferred it until module 2's cap ended it)", m2eReason(it.Reason))
		}
	}
	by := map[string]int{}
	for _, r := range v.Rows {
		by[r.State]++
		if r.State == "pending" && !r.Submitted {
			t.Errorf("row %d is pending and was never stamped as handed over", r.Ordinal)
		}
	}
	if by["pending"] != units || by["malformed"] != 2 || by["duplicate"] != 1 || len(v.Rows) != units+3 || len(v.Results) != units+3 {
		t.Fatalf("the job's rows are %v and it holds %d result(s), want pending %d, malformed 2, duplicate 1 and %d results", by, len(v.Results), units, units+3)
	}
	m.requireModuleJobs(j.ID, units)
	if n := s.count("count the job items of the campaign", `SELECT count(*) FROM public.job_items WHERE campaign_id = $1::uuid`, j.ID); n != units {
		t.Fatalf("public.job_items holds %d row(s) of the campaign, want the %d pending rows and none for the platform's classes", n, units)
	}
	if got := len(v.Msgs); got != 8 {
		t.Fatalf("the sink holds messages for %d recipient(s), want exactly the 8 sent (the repeat of the first recipient made no second message)", got)
	}
	charged := m.requireBooksOfJob(s, tn.UserID, j.ID, pv, units, grant, v)
	s.requireMoves(j.ID, map[string]int64{jobs.StatusConfirmed: 1, jobs.StatusRunning: 1, jobs.StatusCompleting: 1, jobs.StatusSucceeded: 1})
	for _, c := range []struct{ to, by string }{{jobs.StatusConfirmed, "user"}, {jobs.StatusRunning, "engine"}, {jobs.StatusCompleting, "engine"}, {jobs.StatusSucceeded, "engine"}} {
		if n := s.movesBy(j.ID, c.to, c.by); n != 1 {
			t.Errorf("the job moved to %s %d time(s) by %s, want exactly once by %s", c.to, n, c.by, c.by)
		}
	}
	if n := s.eventKindCount(j.ID, "flag") + s.eventKindCount(j.ID, "stale"); n != 0 {
		t.Errorf("the job raised %d flag or stale event(s): nothing may be doubted or invented\n%s", n, strings.Join(s.events(j.ID), "\n"))
	}
	if s.invented(j.ID) != 0 {
		t.Errorf("the job holds an infrastructure_failure or unmapped result")
	}
	jlM2Controls(t, v)
	t.Logf("NOTE jl m2 send: 15 rows (12 run at module 2, 2 malformed P-08, 1 repeat P-07); module 2 reports %d sent, %d bounced, %d failed (attempts_exhausted); the ledger holds %d charge row(s) = %d credits of the %d reserved (remainder %d released once); the sink holds %d message(s), each sent recipient once; results %d; module 2 holds %d job(s)",
		nSent, nBounced, nFailed, len(v.Ledger), charged, pv.Credits, pv.Credits-charged, len(v.Msgs), len(v.Results), units)
}

// ---- cancel ------------------------------------------------------------------------------------------------------------------------------------------------------------------

// TestJobsLive_M2_CancelRunningJob (Phase 4, chunk B10, with the REAL module 2, its worker and the controllable sink): user K sends five rows with the sink holding every send in DATA for 7 s; module 2's one lane takes ONE
// item and the other four stay queued. K cancels the RUNNING job through the store (`running -> cancelling`, as the user: the database accepts it because module 2 can cancel) and the engine, at its next tick, asks
// module 2 to cancel (POST /v1/cancel, repeated at every tick of a cancelling job), records what the module reports and ends the job when the module says every item is final.
//
// WHAT MUST HOLD (jlM2ThreeWay over the report, the results, the ledger and the sink, item by item):
//
//	the partition   the item that was in flight finished and is reported ONCE, as sent; every other item is cancelled; the two make five; the job ends `cancelled`, the cancel request recorded, finished once
//	nothing new     the sink was asked at RCPT TO only about the in-flight recipient, before the cancel and after it; and it does not change AT ALL for five seconds after the job ended (the engine keeps ticking)
//	the money       one charge row (the in-flight item, m2_accepted, at the frozen price), the four cancelled items free (M2-08, no unit); the reserve of five items at the dearest unit was charged one item and the
//	                remainder released ONCE; the balance is the grant less one charge; the books' proofs are empty
//	the moves       the user moved the job to cancelling once, the engine to completing once and to cancelled once; no `flag`, no `stale` event
//
// THE CONTROL (the same job NOT cancelled): user C has the same shape and the same hold; the job ends `succeeded` with EVERY row sent (five messages at the sink, five charge rows, no cancelling move): so the four
// cancellations are the cancel's doing, and the sink check is shown to see messages.
func TestJobsLive_M2_CancelRunningJob(t *testing.T) {
	m := newJlM2Env(t)
	r := m.r
	s, _ := m.stack(r.m2URL, 0)
	const n, grant = m2wHeldItems, 1000
	k := m.tenant(s, "jlcxl")
	c := m.tenant(s, "jlctl")
	s.grant(k.UserID, jobs.ModuleSendManaged, grant)
	s.grant(c.UserID, jobs.ModuleSendManaged, grant)
	kAddrs, kKind := m.recipients(k, slices.Repeat([]string{"hold"}, n))
	cAddrs, cKind := m.recipients(c, slices.Repeat([]string{"hold"}, n))
	r.setMode(t, m2wMode{DataDelayMs: m2wHoldMs, DelayRecipients: append(append([]string(nil), kAddrs...), cAddrs...)})
	diagK := func() string { return r.diagnostics(t, k) }
	diagC := func() string { return r.diagnostics(t, c) }

	// ---- K: five rows, one in flight and held by the sink ----
	kj, kpv, _ := s.start(k.UserID, jobs.CreateReq{Module: jobs.ModuleSendManaged, Rows: jlRowsOf(kAddrs), Content: jlContent("m2 cancel")})
	m.attach(k, kj.ID, s.rows(kj.ID), kKind)
	s.tickToStatus(kj.ID, jobs.StatusRunning, 2*time.Minute, diagK)
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
		if kKind[a] != "hold" {
			t.Fatalf("the sink holds a send to %s: it is not a recipient of K's job", a)
		}
	}
	sort.Strings(flyingList)
	if held.Held[0].RemainingMs < 3500 {
		t.Fatalf("the held send has only %d ms of its hold left: too late to cancel while it is in flight (the hold is %d ms; the runner is slow); nothing was asserted", held.Held[0].RemainingMs, m2wHoldMs)
	}
	if got := r.touched(r.sinkView(t), k); strings.Join(got, ",") != strings.Join(flyingList, ",") {
		t.Fatalf("before the cancel the sink has answered RCPT TO for %v, want exactly the in-flight %v (one worker, one lane: nothing else may have started)", got, flyingList)
	}
	before := m.report(k.UserID, kj.ID)
	if before.Totals.Total != int64(n) || before.Totals.Cancelled != 0 || before.Totals.Pending+before.Totals.WaitingForCapacity < 1 {
		t.Fatalf("before the cancel module 2's totals are (%s), want %d items, none cancelled and at least one still queued", m2eTotalsLine(before.Totals), n)
	}

	// ---- the user cancels the RUNNING job ----
	cj, err := s.store.CancelBeforeStart(r.ctx, k.UserID, kj.ID)
	if err != nil || cj.Status != jobs.StatusCancelling || cj.CancelRequestedAt == nil {
		t.Fatalf("the user's cancel of the running module 2 job answered %+v (error %v), want cancelling with the request recorded: the database accepts it because module 2 can cancel", cj, err)
	}
	s.tickToStatus(kj.ID, jobs.StatusCancelled, 3*time.Minute, diagK)
	endedAt := time.Now()
	for deadline := time.Now().Add(10 * time.Second); r.inflight(t).Inflight > 0; time.Sleep(200 * time.Millisecond) { // the module reported complete, so nothing can still be held
		if time.Now().After(deadline) {
			t.Fatalf("the sink still holds a session 10 s after the job ended: a send was in flight although the module reported every item final")
		}
	}
	atEnd := r.sinkView(t)

	// ---- nothing new reaches the sink after the job ended ----
	for time.Since(endedAt) < 5*time.Second {
		s.eng.Tick(r.ctx) // an ended job is none of the engine's work: a tick must not resubmit, recancel or settle it again
		now := r.sinkView(t)
		if jlM2SinkLine(now) != jlM2SinkLine(atEnd) {
			t.Fatalf("the sink changed after the job ended and the module reported complete: %s -> %s", jlM2SinkLine(atEnd), jlM2SinkLine(now))
		}
		time.Sleep(500 * time.Millisecond)
	}

	// ---- the three places ----
	v := m.view(s, k, kj.ID)
	if problems := jlM2ThreeWay(v); len(problems) != 0 {
		t.Fatalf("module 2's report, the results, the ledger and the sink disagree (%d):\n  %s", len(problems), strings.Join(problems, "\n  "))
	}
	var sentAddr, cancelledAddr []string
	rowOf := map[string]string{}
	for _, row := range v.Rows {
		rowOf[row.ID] = row.Norm
	}
	for _, it := range v.Report {
		switch it.State {
		case "sent":
			sentAddr = append(sentAddr, rowOf[it.JobItemID])
		case "cancelled":
			cancelledAddr = append(cancelledAddr, rowOf[it.JobItemID])
		default:
			t.Fatalf("an item is reported %s after the cancel: want cancelled or sent", it.State)
		}
	}
	sort.Strings(sentAddr)
	if strings.Join(sentAddr, ",") != strings.Join(flyingList, ",") || len(cancelledAddr) < 1 || len(sentAddr)+len(cancelledAddr) != n {
		t.Fatalf("the partition is sent %v and %d cancelled, want the in-flight %v sent (it finished, reported once) and the other %d cancelled", sentAddr, len(cancelledAddr), flyingList, n-len(flyingList))
	}
	if got := r.touched(atEnd, k); strings.Join(got, ",") != strings.Join(flyingList, ",") {
		t.Fatalf("at the end the sink has answered RCPT TO for %v, want only the in-flight %v: something was sent after the cancel", got, flyingList)
	}
	if len(atEnd.Msgs) != len(flyingList) {
		t.Fatalf("the sink holds messages for %d recipient(s), want %d: the in-flight item reported exactly once and nothing for a cancelled one", len(atEnd.Msgs), len(flyingList))
	}
	m.requireModuleJobs(kj.ID, n)
	final, err := s.store.Get(r.ctx, k.UserID, kj.ID)
	if err != nil || final.Status != jobs.StatusCancelled || final.FinishedAt == nil || final.CancelRequestedAt == nil {
		t.Fatalf("K's job is %+v (error %v), want cancelled with a finish time and the cancel request", final, err)
	}
	charged := m.requireBooksOfJob(s, k.UserID, kj.ID, kpv, n, grant, v)
	if want := int64(len(sentAddr)) * int64(v.Prices["m2_accepted"]); charged != want || int64(len(v.Ledger)) != int64(len(sentAddr)) {
		t.Fatalf("K was charged %d credits in %d charge row(s), want %d for the %d item(s) that finished and nothing for the cancelled ones", charged, len(v.Ledger), want, len(sentAddr))
	}
	for _, rs := range v.Results {
		if rs.State == "cancelled" && (rs.Decision != "free" || rs.Unit != "" || rs.Rule != "M2-08") {
			t.Errorf("a cancelled item's result is %+v, want free, no unit, M2-08", rs)
		}
	}
	s.requireMoves(kj.ID, map[string]int64{jobs.StatusConfirmed: 1, jobs.StatusRunning: 1, jobs.StatusCancelling: 1, jobs.StatusCompleting: 1, jobs.StatusCancelled: 1})
	for _, x := range []struct{ to, by string }{{jobs.StatusCancelling, "user"}, {jobs.StatusCompleting, "engine"}, {jobs.StatusCancelled, "engine"}} {
		if got := s.movesBy(kj.ID, x.to, x.by); got != 1 {
			t.Errorf("K's job moved to %s %d time(s) by %s, want exactly once by %s", x.to, got, x.by, x.by)
		}
	}
	if x := s.eventKindCount(kj.ID, "flag") + s.eventKindCount(kj.ID, "stale"); x != 0 {
		t.Errorf("K's job raised %d flag or stale event(s): the cancel must end the job without the engine inventing or doubting anything\n%s", x, strings.Join(s.events(kj.ID), "\n"))
	}
	// the control of the comparison and of the sink check: a cancelled item that reached the sink, and a cancelled item reported sent, are both caught
	w := v.clone()
	w.Msgs[cancelledAddr[0]], w.Rcpt[cancelledAddr[0]] = 1, m2wRcpt{Accepted: 1}
	jlWantProblem(t, "control: a cancelled item that reached the sink", "the sink holds 1 message(s)", jlM2ThreeWay(w))
	jlM2Controls(t, v)
	t.Logf("NOTE jl m2 cancel K: the user cancelled the running job; module 2 cancelled %d of %d (queued), the in-flight item finished and is reported once as sent; the job ended cancelled; the sink was asked at RCPT TO about the in-flight recipient only and did not change for 5 s after the end; one charge row of %d credit(s); reserve %d, remainder %d released once; results %d", len(cancelledAddr), n, charged, kpv.Credits, kpv.Credits-charged, len(v.Results))

	// ---- the control: the same job, never cancelled ----
	cj2, cpv, _ := s.start(c.UserID, jobs.CreateReq{Module: jobs.ModuleSendManaged, Rows: jlRowsOf(cAddrs), Content: jlContent("m2 cancel control")})
	m.attach(c, cj2.ID, s.rows(cj2.ID), cKind)
	s.tickToStatus(cj2.ID, jobs.StatusRunning, 2*time.Minute, diagC)
	r.waitInflight(t, 1, 60*time.Second)
	mid := m.report(c.UserID, cj2.ID)
	if mid.Totals.Total != int64(n) || mid.Totals.Cancelled != 0 || mid.Totals.Sent >= int64(n) || s.status(cj2.ID) != jobs.StatusRunning {
		t.Fatalf("while the hold is on the control's report is (%s) and the job %s: want %d items, none cancelled, fewer than %d sent and the job running", m2eTotalsLine(mid.Totals), s.status(cj2.ID), n, n)
	}
	s.tickToStatus(cj2.ID, jobs.StatusSucceeded, 6*time.Minute, diagC)
	cv := m.view(s, c, cj2.ID)
	if problems := jlM2ThreeWay(cv); len(problems) != 0 {
		t.Fatalf("the control job: module 2's report, the results, the ledger and the sink disagree (%d):\n  %s", len(problems), strings.Join(problems, "\n  "))
	}
	for _, rs := range cv.Results {
		if rs.State != "sent" || rs.Rule != "M2-01" || rs.Unit != "m2_accepted" {
			t.Fatalf("a result of the control job is %+v, want sent by M2-01 charged m2_accepted: without a cancel every row is sent", rs)
		}
	}
	if len(cv.Msgs) != n || len(cv.Ledger) != n || len(cv.Results) != n {
		t.Fatalf("the control job holds %d result(s) and %d charge row(s) and the sink %d message(s), want %d of each: without a cancel every row is sent exactly once", len(cv.Results), len(cv.Ledger), len(cv.Msgs), n)
	}
	cCharged := m.requireBooksOfJob(s, c.UserID, cj2.ID, cpv, n, grant, cv)
	if moved := s.moves(cj2.ID, jobs.StatusCancelling); moved != 0 {
		t.Fatalf("the control job moved to cancelling %d time(s)", moved)
	}
	s.requireMoves(cj2.ID, map[string]int64{jobs.StatusConfirmed: 1, jobs.StatusRunning: 1, jobs.StatusCompleting: 1, jobs.StatusSucceeded: 1})
	t.Logf("NOTE jl m2 cancel control C: the same job, never cancelled: while the hold was on %d of %d were sent and none cancelled; the job ended succeeded with %d message(s) at the sink and %d charge row(s) = %d credits", mid.Totals.Sent, n, len(cv.Msgs), len(cv.Ledger), cCharged)
}

// jlM2SinkLine renders everything the sink says of this run's recipients (messages and RCPT counters), so that two readings can be compared and a difference printed.
func jlM2SinkLine(v m2wView) string {
	var parts []string
	for a, ms := range v.Msgs {
		parts = append(parts, fmt.Sprintf("%s:msgs=%d", a, len(ms)))
	}
	for a, c := range v.Rcpt {
		parts = append(parts, fmt.Sprintf("%s:rcpt=%d/%d/%d", a, c.Accepted, c.Rejected, c.Deferred))
	}
	sort.Strings(parts)
	return strings.Join(parts, " ")
}

// ---- outage --------------------------------------------------------------------------------------------------------------------------------------------------------------

// jlM2OutageItems is the number of recipients of a module 2 outage job (ten the sink accepts, two it rejects); its list also holds two malformed rows and a repeat.
const jlM2OutageItems = 12

// jlRunM2Outage runs one module 2 job through the engine against the link. With cut set the link is cut after the first results are recorded and held down (holdOutage), then reopened; without it the link is never
// cut and the job must gain results (and charges) in the same window (the paired control).
func (m *jlM2Env) runOutage(s *jlStack, tr *jlTrace, link *jlLink, tn *m2wTenant, addrs []string, kindOf map[string]string, cut bool) {
	t := m.t
	t.Helper()
	const n, grant = jlM2OutageItems, 1000
	what := "outage control (never cut)"
	if cut {
		what = "outage"
	}
	diag := func() string { return m.r.diagnostics(t, tn) }
	s.grant(tn.UserID, jobs.ModuleSendManaged, grant)
	list := jlBuildList(addrs, 2, 1)
	j, pv, res := s.start(tn.UserID, jobs.CreateReq{Module: jobs.ModuleSendManaged, Rows: list.Rows, Content: jlContent("m2 " + what)})
	if pv.Rows != n+3 || pv.Pending != n || pv.Malformed != 2 || pv.Duplicates != 1 || pv.Units != n || !pv.Metered || res.Job.Status != jobs.StatusConfirmed || res.Reserved != pv.Credits || res.Pending != n {
		t.Fatalf("the preview counts rows %d pending %d malformed %d duplicates %d units %d metered %v and the confirm answered %s reserved %d pending %d; want %d, %d, 2, 1, %d, metered, confirmed, %d, %d",
			pv.Rows, pv.Pending, pv.Malformed, pv.Duplicates, pv.Units, pv.Metered, res.Job.Status, res.Reserved, res.Pending, n+3, n, n, pv.Credits, n)
	}
	m.attach(tn, j.ID, s.rows(j.ID), kindOf)
	s.tickToStatus(j.ID, jobs.StatusRunning, 3*time.Minute, diag)

	s.tickUntil("the engine has recorded the first results of module 2 while most of the job is still to come", 4*time.Minute, 400*time.Millisecond, diag, func() (bool, string) {
		_, mod := s.resultCount(j.ID)
		if mod > n-4 {
			t.Fatalf("module 2 had settled %d of the %d items by the first time the engine recorded any: too fast for a cut in the middle of the job (the sink holds every accepted send for 2 s)\n%s", mod, n, s.dump(diag))
		}
		return mod >= 2, fmt.Sprintf("%d result(s) of the module recorded", mod)
	})
	all0, mod0 := s.resultCount(j.ID)
	if all0 != mod0+3 {
		t.Fatalf("before the cut the job holds %d result(s), %d of them the module's: want the module's plus the platform's three (2 malformed, 1 repeat)", all0, mod0)
	}
	link.requireCarried("before the cut")

	var held jlHeld
	if cut {
		link.requireUp("before the cut")
		link.Cut()
		link.requireDown("just after the cut")
		held = s.holdOutage(jlHold{
			Job: j.ID, Link: link, Tr: tr, Diag: diag,
			Books: s.booksOf(j.ID, tn.UserID, jobs.ModuleSendManaged),
			Ahead: func() (bool, string) {
				rep := m.report(tn.UserID, j.ID) // at the module's own address: the test's reader never goes through the forwarder
				return rep.Progress.Terminal > mod0, fmt.Sprintf("module 2's own report says %d terminal item(s), the platform recorded %d", rep.Progress.Terminal, mod0)
			},
		})
		sinkDuring := len(m.r.sinkView(t).Msgs)
		link.mustOpen()
		link.requireUp("after the link was reopened")
		t.Logf("NOTE jl outage m2: the link to module 2 was cut with %d of %d module results recorded; for %s (%d ticks) the job stayed running, gained no result, no charge row and no event but %d stale event(s), the reserve and the balance did not move, the engine's %d observation(s) all came back as waits, and the link refused every probe; meanwhile module 2 went on: its own report says %s, the sink holds messages for %d recipient(s)",
			mod0, n, held.Held.Round(time.Second), held.Ticks, held.Stale, held.Waits, held.Ahead, sinkDuring)
	} else {
		link.requireUp("the control, before its window")
		s.tickUntil("with the link open the platform gains results in the window the outage job gained none in", 90*time.Second, 700*time.Millisecond, diag, func() (bool, string) {
			_, mod := s.resultCount(j.ID)
			return mod > mod0, fmt.Sprintf("%d result(s) of the module recorded, %d at the start of the window", mod, mod0)
		})
	}
	s.tickToStatus(j.ID, jobs.StatusSucceeded, 5*time.Minute, diag)

	// ---- after: the job completed exactly once and the three places agree ----
	v := m.view(s, tn, j.ID)
	if problems := jlM2ThreeWay(v); len(problems) != 0 {
		t.Fatalf("module 2's report, the results, the ledger and the sink disagree (%d):\n  %s", len(problems), strings.Join(problems, "\n  "))
	}
	nSent, nBounced, nFailed, nCancelled := jlM2Totals(v.Report)
	if nSent != 10 || nBounced != 2 || nFailed != 0 || nCancelled != 0 || len(v.Report) != n || len(v.Results) != n+3 || len(v.Msgs) != 10 {
		t.Fatalf("module 2 reports %d sent, %d bounced, %d failed, %d cancelled (%d item(s)), the job holds %d result(s) and the sink messages for %d recipient(s); want 10, 2, 0, 0 (%d), %d and 10: every recipient exactly once, the items finished during the outage recorded after it",
			nSent, nBounced, nFailed, nCancelled, len(v.Report), len(v.Results), len(v.Msgs), n, n+3)
	}
	m.requireModuleJobs(j.ID, n)
	charged := m.requireBooksOfJob(s, tn.UserID, j.ID, pv, n, grant, v)
	s.requireMoves(j.ID, map[string]int64{jobs.StatusConfirmed: 1, jobs.StatusRunning: 1, jobs.StatusCompleting: 1, jobs.StatusSucceeded: 1})
	for _, c := range []struct{ to, by string }{{jobs.StatusConfirmed, "user"}, {jobs.StatusRunning, "engine"}, {jobs.StatusCompleting, "engine"}, {jobs.StatusSucceeded, "engine"}} {
		if got := s.movesBy(j.ID, c.to, c.by); got != 1 {
			t.Errorf("the job moved to %s %d time(s) by %s, want exactly once by %s", c.to, got, c.by, c.by)
		}
	}
	if f := s.eventKindCount(j.ID, "flag"); f != 0 {
		t.Errorf("the job raised %d flag event(s): nothing may be doubted or invented\n%s", f, strings.Join(s.events(j.ID), "\n"))
	}
	stale := s.eventKindCount(j.ID, "stale")
	switch {
	case cut && stale < 1:
		t.Errorf("the outage job raised no stale event (the dial is %s and the link was down for %s)", jlOutageStale, held.Held.Round(time.Second))
	case !cut && stale != 0:
		t.Errorf("the control job (link never cut, the default staleness dial) raised %d stale event(s): it made progress all along", stale)
	}
	if s.invented(j.ID) != 0 {
		t.Errorf("the job holds an infrastructure_failure or unmapped result")
	}
	if cut {
		t.Logf("NOTE jl outage m2 result: after the link was reopened the job went from %d to %d module results and ended succeeded: completing x1, succeeded x1, no failed move, no flag; the ledger holds %d charge row(s) = %d credits (the report implies exactly that), the remainder %d released once; module 2 holds %d job(s), the sink messages for %d recipient(s), each once",
			mod0, n, len(v.Ledger), charged, pv.Credits-charged, n, len(v.Msgs))
	} else {
		t.Logf("NOTE jl outage m2 control: the same job with the link never cut gained results and charges in the window, raised no stale event and ended succeeded the same way: %d charge row(s) = %d credits, the sink messages for %d recipient(s)", len(v.Ledger), charged, len(v.Msgs))
	}
}

// TestJobsLive_Outage_M2_HaltsCleanlyAndCompletesOnce (Phase 4, chunk B9, the outage criterion for module 2, METERED): a module 2 send job (ten recipients the sink accepts, two it rejects with 550, two malformed rows and a
// repeat) is running, some items settled and charged and more in flight (the sink holds every accepted send for 2 s), when the platform's path to module 2 is cut (jlLink). For at least 20 seconds of engine ticks, with the
// link refusing every probe: the job stays running; platform.job_results gains NO row; the ledger gains NO charge row and the reserve and the balance do not move (the books are fingerprinted at every tick); no
// result is invented; the job's events since the cut are stale events and nothing else; the engine's observations of module 2 all come back as waits; a `stale` event appears and the job is STILL running; and module 2
// meanwhile finishes more items than the platform recorded (its own report, read directly).
//
// The link is reopened and the job reaches `succeeded` EXACTLY once: one move to completing and one to succeeded, a result for every row, ONE charge row per sent and per bounced item at the frozen price (jlM2ThreeWay:
// the ledger equals what module 2's own report implies, item by item, and the sink holds each sent recipient once), the reserve charged that and the remainder released once. THE PAIRED CONTROL is the same job for a
// second user with the link never cut and the default staleness dial: results and charges are gained in the window the outage job gained none in, no stale event is raised, the job ends the same way.
func TestJobsLive_Outage_M2_HaltsCleanlyAndCompletesOnce(t *testing.T) {
	m := newJlM2Env(t)
	link := newJlLink(t, "module 2", m.r.m2URL)
	sOut, trOut := m.stack(link.URL(), jlOutageStale)
	sCtl, trCtl := m.stack(link.URL(), 0) // the default staleness dial: 15 minutes
	// registered after the stacks, so it runs BEFORE their cleanups and before the run's own (the campaigns are cancelled directly): a failed test's campaigns are ended through an open link
	t.Cleanup(func() {
		if err := link.Open(); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	link.requireUp("at the start")
	k := m.tenant(sOut, "jlout")
	c := m.tenant(sCtl, "jlctl")
	kinds := []string{"ok", "ok", "bounce", "ok", "ok", "ok", "ok", "bounce", "ok", "ok", "ok", "ok"}
	kAddrs, kKind := m.recipients(k, kinds)
	cAddrs, cKind := m.recipients(c, kinds)
	rejected := append(jlOfKind(kAddrs, kKind, "bounce"), jlOfKind(cAddrs, cKind, "bounce")...)
	delayed := append(jlOfKind(kAddrs, kKind, "ok"), jlOfKind(cAddrs, cKind, "ok")...)
	m.r.setMode(t, m2wMode{RejectRcpt: rejected, RejectCode: 550, DataDelayMs: 2000, DelayRecipients: delayed})

	m.runOutage(sOut, trOut, link, k, kAddrs, kKind, true)
	m.runOutage(sCtl, trCtl, link, c, cAddrs, cKind, false)
}
