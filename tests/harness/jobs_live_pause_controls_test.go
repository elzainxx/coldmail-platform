//go:build harness

package harness

import (
	"fmt"
	"testing"

	m1ad "github.com/coldmail-platform/platform/internal/adapters/m1"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
)

// THE CONTROLS OF THE PAUSE AND CANCEL COMPARISONS (TestJobsLive_ComparisonCanFail_PauseCancel): an ordinary harness test that needs no stack. The live pause and cancel tests (jobs_live_pause_m3_test.go,
// m2_worker_jobs_pause_test.go, jobs_live_m1cancel_test.go) decide that a job in the middle of its life, or a cancelled one, is consistent in its three places with four comparisons of their own: jlpcProblems (the
// platform's rows and results, the module's own report, the sink and the ledger at one instant), jlpcSinkDiff (what changed at the sink between two readings: the instrument of "nothing new reaches the sink
// while the job is held"), jlpcNeverHandedAsCancelled with jlM3ThreeWay (a job whose rows were partly never handed to the module) and jlpcM1CancelThreeWay (a module 1 job that was cancelled). Each is shown here to accept a
// consistent view and to report each of the ways it can be wrong, naming the reason: a comparison that cannot fail would let the live tests go green over a platform that holds, cancels or settles wrongly.

// jlpcSynthetic is a consistent reading of a send job in the middle of its life: row 1 pending and sent (one message, one acceptance), row 2 pending and queued at the module, row 3 pending and never handed over,
// row 4 malformed (the platform's own), row 5 pending and cancelled at the module. For a metered module the sent row is charged one unit of m2_accepted at the price 2 and the ledger says so.
func jlpcSynthetic(module string, metered bool) jlpcReading {
	addr := func(n int) string { return fmt.Sprintf("jlpc-ctl-%d@example.test", n) }
	row := func(n int, state string) jlRow {
		return jlRow{ID: jlID(n), Ordinal: n, State: state, Norm: addr(n), Submitted: state == "pending"}
	}
	rows := []jlRow{row(1, "pending"), row(2, "pending"), row(3, "pending"), {ID: jlID(4), Ordinal: 4, State: "malformed", Reason: "no_at"}, row(5, "pending")}
	rows[2].Submitted = false
	report := []sendapi.Item{
		{JobItemID: jlID(1), State: "sent", Class: sendapi.ClassTerminal},
		{JobItemID: jlID(2), State: "queued", Class: sendapi.ClassPending},
		{JobItemID: jlID(5), State: "cancelled", Class: sendapi.ClassTerminal},
	}
	unit, decision := "", "free"
	if metered {
		unit, decision = "m2_accepted", "charge"
	}
	results := []jlResult{
		{Ref: jlID(1), Ordinal: 1, Email: addr(1), Origin: "module", State: "sent", Decision: decision, Unit: unit, Rule: jlpcRule(module, "sent")},
		{Ref: jlID(4), Ordinal: 4, Email: "no-at-sign", Origin: "platform", State: "malformed", Reason: "no_at", Decision: "free", Rule: "P-08"},
		{Ref: jlID(5), Ordinal: 5, Email: addr(5), Origin: "module", State: "cancelled", Decision: "free", Rule: jlpcRule(module, "cancelled")},
	}
	v := jlpcReading{
		Module: module, Metered: metered, Rows: rows, Results: results, Report: report,
		Sink: jlpcSink{Msgs: map[string]int{addr(1): 1}, Rcpt: map[string]jlpcRcpt{addr(1): {Accepted: 1}}},
	}
	if metered {
		v.Ledger = []m1bCharge{{ItemRef: jlID(1), Unit: "m2_accepted", Cost: 2, Units: 1}}
		v.Prices = map[string]int{"m2_accepted": 2, "m2_bounced": 1}
	}
	return v
}

func (v jlpcReading) clone() jlpcReading {
	w := v
	w.Rows = append([]jlRow(nil), v.Rows...)
	w.Results = append([]jlResult(nil), v.Results...)
	w.Report = append([]sendapi.Item(nil), v.Report...)
	w.Before = append([]sendapi.Item(nil), v.Before...)
	w.Ledger = append([]m1bCharge(nil), v.Ledger...)
	w.Sink = jlpcSink{Msgs: map[string]int{}, Rcpt: map[string]jlpcRcpt{}}
	for k, x := range v.Sink.Msgs {
		w.Sink.Msgs[k] = x
	}
	for k, x := range v.Sink.Rcpt {
		w.Sink.Rcpt[k] = x
	}
	return w
}

// TestJobsLive_ComparisonCanFail_PauseCancel: every comparison of the pause and cancel live tests accepts a consistent view and reports each way it can be wrong.
func TestJobsLive_ComparisonCanFail_PauseCancel(t *testing.T) {
	a := func(n int) string { return fmt.Sprintf("jlpc-ctl-%d@example.test", n) }
	type tamper struct {
		name, mention string
		mutate        func(v *jlpcReading)
	}
	both := []tamper{
		{"a row never handed to the module has a result", "was not handed to the module and has 1 result(s)", func(v *jlpcReading) {
			v.Results = append(v.Results, jlResult{Ref: jlID(3), Ordinal: 3, Email: a(3), Origin: "module", State: "sent", Decision: "free"})
		}},
		{"a row never handed to the module has a message at the sink", "was not handed to the module and the sink holds 1 message(s)", func(v *jlpcReading) { v.Sink.Msgs[a(3)] = 1 }},
		{"a queued row has a result", "(not final) and has 1 result(s)", func(v *jlpcReading) {
			v.Results = append(v.Results, jlResult{Ref: jlID(2), Ordinal: 2, Email: a(2), Origin: "module", State: "sent", Decision: "free"})
		}},
		{"a queued row has two messages at the sink", "the sink holds 2 messages for it", func(v *jlpcReading) { v.Sink.Msgs[a(2)] = 2 }},
		{"a sent row has no message at the sink", "is sent: the sink holds 0 message(s)", func(v *jlpcReading) { v.Sink.Msgs[a(1)] = 0 }},
		{"a sent row was never accepted at RCPT", "want one message and one acceptance", func(v *jlpcReading) { v.Sink.Rcpt[a(1)] = jlpcRcpt{} }},
		{"a cancelled row reached the sink", "is cancelled: the sink holds 1 message(s)", func(v *jlpcReading) { v.Sink.Msgs[a(5)] = 1 }},
		{"a cancelled row was asked at RCPT", "is cancelled: the sink holds 0 message(s) and answered RCPT", func(v *jlpcReading) { v.Sink.Rcpt[a(5)] = jlpcRcpt{Accepted: 1} }},
		{"a result's state differs from the module's report", "the result says bounced from module", func(v *jlpcReading) { v.Results[0].State = "bounced" }},
		{"a result carries another rule", "the result is under rule", func(v *jlpcReading) { v.Results[0].Rule = "X-99" }},
		{"a result of the module is claimed by the platform", "the result says sent from platform", func(v *jlpcReading) { v.Results[0].Origin = "platform" }},
		{"a final item has no result although it was final at the earlier read", "recorded no result for it since", func(v *jlpcReading) {
			v.Results = v.Results[1:]
			v.Ledger = nil
			v.Before = []sendapi.Item{{JobItemID: jlID(1), State: "sent", Class: sendapi.ClassTerminal}}
		}},
		{"an item that was final at the earlier read is not final now", "was final at the earlier read and is queued now", func(v *jlpcReading) {
			v.Before = []sendapi.Item{{JobItemID: jlID(2), State: "sent", Class: sendapi.ClassTerminal}}
		}},
		{"a platform row has no result", "has 0 result(s)", func(v *jlpcReading) { v.Results = append(v.Results[:1:1], v.Results[2:]...) }},
		{"a platform row's result is the module's", "want platform", func(v *jlpcReading) { v.Results[1].Origin = "module" }},
		{"the module's report holds an item for a platform row", "the platform ended it and never ran it", func(v *jlpcReading) {
			v.Report = append(v.Report, sendapi.Item{JobItemID: jlID(4), State: "queued", Class: sendapi.ClassPending})
		}},
		{"a result belongs to no row", "belong to a row the job does not have", func(v *jlpcReading) {
			v.Results = append(v.Results, jlResult{Ref: jlID(99), Origin: "module", State: "sent", Decision: "free"})
		}},
		{"the module's report holds an item that is no row", "that is no row of the job", func(v *jlpcReading) {
			v.Report = append(v.Report, sendapi.Item{JobItemID: jlID(98), State: "queued", Class: sendapi.ClassPending})
		}},
		{"a final item in a state no step of the scenarios can make", "which no step of these scenarios can make", func(v *jlpcReading) { v.Report[0].State = "failed" }},
	}
	free := []tamper{
		{"a free sender's result names a unit", "the result says decision", func(v *jlpcReading) { v.Results[0].Unit = "m2_accepted" }},
	}
	metered := []tamper{
		{"a charge dropped from the ledger", "the ledger holds 0 charge row(s) for it, want exactly 1", func(v *jlpcReading) { v.Ledger = nil }},
		{"a charge at the wrong price", "the frozen price of m2_accepted is 2", func(v *jlpcReading) { v.Ledger[0].Cost = 5 }},
		{"a charge in the wrong unit", "the table says one unit of m2_accepted", func(v *jlpcReading) { v.Ledger[0].Unit = "m2_bounced" }},
		{"a charge for a queued row", "(not final) and the ledger holds a charge for it", func(v *jlpcReading) {
			v.Ledger = append(v.Ledger, m1bCharge{ItemRef: jlID(2), Unit: "m2_accepted", Cost: 2, Units: 1})
		}},
		{"a charge for a cancelled row", "is a free cancelled item and the ledger holds 1 charge row(s)", func(v *jlpcReading) {
			v.Ledger = append(v.Ledger, m1bCharge{ItemRef: jlID(5), Unit: "m2_accepted", Cost: 2, Units: 1})
		}},
		{"a charge for a row the module does not hold", "was not handed to the module and the ledger holds a charge for it", func(v *jlpcReading) {
			v.Ledger = append(v.Ledger, m1bCharge{ItemRef: jlID(3), Unit: "m2_accepted", Cost: 2, Units: 1})
		}},
		{"a charge with no result", "has a charge in the ledger and no result", func(v *jlpcReading) { v.Results = v.Results[1:] }},
		{"a charged item's result is free", `the table says "charge"`, func(v *jlpcReading) { v.Results[0].Decision, v.Results[0].Unit = "free", "" }},
		{"a charge for an item that is no row of the job", "the ledger holds a charge for an item that is no row of the job", func(v *jlpcReading) {
			v.Ledger = append(v.Ledger, m1bCharge{ItemRef: jlID(99), Unit: "m2_accepted", Cost: 2, Units: 1})
		}},
	}
	for _, c := range []struct {
		module  string
		metered bool
		cases   []tamper
	}{{"m3", false, append(append([]tamper(nil), both...), free...)}, {"m2", true, append(append([]tamper(nil), both...), metered...)}} {
		base := jlpcSynthetic(c.module, c.metered)
		if p := jlpcProblems(base); len(p) != 0 {
			t.Fatalf("control (%s): the consistent reading must pass, got %v", c.module, p)
		}
		for _, tc := range c.cases {
			w := base.clone()
			tc.mutate(&w)
			jlWantProblem(t, c.module+": "+tc.name, tc.mention, jlpcProblems(w))
		}
	}

	// the instrument of the window: the same function finds nothing in an unchanged sink and each kind of change in a changed one
	addrs := []string{a(1), a(2)}
	base := jlpcSink{Msgs: map[string]int{a(1): 1}, Rcpt: map[string]jlpcRcpt{a(1): {Accepted: 1}}}
	same := jlpcSink{Msgs: map[string]int{a(1): 1}, Rcpt: map[string]jlpcRcpt{a(1): {Accepted: 1}}}
	if d := jlpcSinkDiff(base, same, addrs); len(d) != 0 {
		t.Fatalf("control: an unchanged sink reads as changed: %v", d)
	}
	for name, cur := range map[string]jlpcSink{
		"a second message to a recipient":      {Msgs: map[string]int{a(1): 2}, Rcpt: map[string]jlpcRcpt{a(1): {Accepted: 1}}},
		"a first message to another recipient": {Msgs: map[string]int{a(1): 1, a(2): 1}, Rcpt: map[string]jlpcRcpt{a(1): {Accepted: 1}}},
		"a RCPT answer with no message":        {Msgs: map[string]int{a(1): 1}, Rcpt: map[string]jlpcRcpt{a(1): {Accepted: 1}, a(2): {Deferred: 1}}},
	} {
		jlWantProblem(t, "the sink diff: "+name, "recipient", jlpcSinkDiff(base, cur, addrs))
	}
	if d := jlpcSinkDiff(base, jlpcSink{Msgs: map[string]int{a(1): 1, "jlpc-someone-else@example.test": 4}, Rcpt: map[string]jlpcRcpt{a(1): {Accepted: 1}}}, addrs); len(d) != 0 {
		t.Fatalf("control: the sink diff reads another job's recipients as this job's: %v", d)
	}

	// a job whose rows were partly never handed to the module and that was cancelled: the module 3 comparison needs the platform's rows counted as the platform's
	m3 := jlM3Synthetic()
	m3.Rows[4].Submitted = false // row 5 was never handed over: the platform ended it
	m3.Report = m3.Report[:4]
	delete(m3.Jobs, jlID(5))
	delete(m3.Counts, jlpcA3(5))
	delete(m3.Rcpt, jlpcA3(5))
	m3.Results[4] = jlResult{Ref: jlID(5), Ordinal: 5, Email: jlpcA3(5), Origin: "platform", State: "cancelled", Decision: "free", Rule: "P-01"}
	jlWantProblem(t, "a row never handed over, read as a pending row", "row 5 is pending and module 3's report holds 0 item(s) for it", jlM3ThreeWay(m3))
	cancelled := m3.clone()
	cancelled.Rows = jlpcNeverHandedAsCancelled(m3.Rows)
	if p := jlM3ThreeWay(cancelled); len(p) != 0 {
		t.Fatalf("control: a cancelled job whose last row was never handed over is consistent, got %v", p)
	}
	if cancelled.Rows[4].State != "cancelled" || cancelled.Rows[3].State != "pending" || m3.Rows[4].State != "pending" {
		t.Fatalf("jlpcNeverHandedAsCancelled changed the wrong rows or the rows it was given")
	}
	w := cancelled.clone()
	w.Counts[jlpcA3(5)] = 1
	jlWantProblem(t, "a message to the row that was never handed over", "no pending row of the job owns", jlM3ThreeWay(w))
	w = cancelled.clone()
	w.Results[4].Rule = "M3-01"
	jlWantProblem(t, "the platform's cancelled result carries the module's rule", "under rule", jlM3ThreeWay(w))

	// a module 1 job that was cancelled: row 8 is a pending row module 1 cancelled before its check began
	m1 := jlM1Synthetic()
	m1.Rows = append(m1.Rows, jlRow{ID: jlID(8), Ordinal: 8, State: "pending", Norm: "row8@example.test", Submitted: true})
	m1.Report = append(m1.Report, m1ad.Row{Ordinal: 7, ItemID: jlID(8), Status: m1ad.StatusCancelled, Reason: m1ad.ReasonCancelledByCaller})
	m1.Results = append(m1.Results, jlResult{Ref: jlID(8), Ordinal: 8, Email: "row8@example.test", Origin: "module", State: m1ad.StatusCancelled, Reason: m1ad.ReasonCancelledByCaller, Decision: "free", Rule: "M1-24"})
	if p := jlM1ThreeWay(m1); len(p) == 0 {
		t.Fatalf("control: the settlement comparison for a job that was not cancelled accepts a cancelled row it has no word for")
	}
	if p := jlpcM1CancelThreeWay(m1); len(p) != 0 {
		t.Fatalf("control: the cancelled job's consistent view must pass, got %v", p)
	}
	for _, c := range []struct {
		name, mention string
		mutate        func(v *jlM1View)
	}{
		{"a cancelled row's result is charged", "the hand-written oracle says", func(v *jlM1View) { v.Results[7].Decision = "charge"; v.Results[7].Unit = "m1_own" }},
		{"a cancelled row's result names a unit", "the result names unit", func(v *jlM1View) { v.Results[7].Unit = "m1_own" }},
		{"a cancelled row's result carries another rule", "want M1-24", func(v *jlM1View) { v.Results[7].Rule = "M1-03" }},
		{"a cancelled row's result says a verdict", "module 1's report says cancelled/cancelled_by_caller", func(v *jlM1View) { v.Results[7].State = "valid"; v.Results[7].Reason = "accepted" }},
		{"a cancelled row has no result", "has 0 result(s)", func(v *jlM1View) { v.Results = v.Results[:7] }},
		{"a cancelled row is charged in the ledger", "the ledger holds a charge for a row module 1 cancelled", func(v *jlM1View) {
			v.Ledger = append(v.Ledger, m1bCharge{ItemRef: jlID(8), Unit: "m1_own", Cost: 1, Units: 1})
		}},
		{"a row with a verdict carries the cancelled rule", "the rule of a cancelled row", func(v *jlM1View) { v.Results[0].Rule = "M1-24" }},
		{"a cancelled row's reason is another word", "the hand-written oracle says", func(v *jlM1View) { v.Report[4].Reason = "something_else" }},
	} {
		w := m1.clone()
		c.mutate(&w)
		jlWantProblem(t, "module 1 cancel: "+c.name, c.mention, jlpcM1CancelThreeWay(w))
	}
}

// jlpcA3 is the address of row n of jlM3Synthetic.
func jlpcA3(n int) string { return fmt.Sprintf("m3a-ctl-%d@example.test", n) }
