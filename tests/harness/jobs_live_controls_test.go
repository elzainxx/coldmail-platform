//go:build harness

package harness

import (
	"fmt"
	"strings"
	"testing"

	m1ad "github.com/coldmail-platform/platform/internal/adapters/m1"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/norm"
)

// THE CONTROLS OF THE LIVE JOB TESTS' COMPARISONS (TestJobsLive_ComparisonCanFail_*): ordinary harness tests that need no stack. Each builds a small view that is CONSISTENT (the comparison returns nothing), then
// breaks it one way at a time and requires the comparison to report that break, naming the reason: a comparison that cannot fail would let the live tests go green over a platform that settles wrongly. They also
// pin the lists the live tests send against the platform's own address rule (norm.Validate), so that a list that does not classify as the tests assume is red here and not after a half-hour harness run.

// jlID is a row id of the synthetic views.
func jlID(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", n) }

// jlM1Synthetic is a consistent view of a module 1 job: seven rows (a valid answer, an unknown caused by us, an unknown caused by the recipient's server, an invalid answer, a malformed row, a repeat and a
// suppressed row), the report of the four that were sent, the results, and the ledger of the three that are charged.
func jlM1Synthetic() jlM1View {
	row := func(n int, state string) jlRow {
		return jlRow{ID: jlID(n), Ordinal: n, State: state, Norm: fmt.Sprintf("row%d@example.test", n), Submitted: state == "pending"}
	}
	rows := []jlRow{row(1, "pending"), row(2, "pending"), row(3, "pending"), row(4, "malformed"), row(5, "duplicate"), row(6, "suppressed"), row(7, "pending")}
	report := []m1ad.Row{
		{Ordinal: 0, ItemID: jlID(1), Status: "valid", Reason: "accepted"},
		{Ordinal: 1, ItemID: jlID(2), Status: "unknown", Reason: "dns_error", FailureOrigin: "own"},
		{Ordinal: 2, ItemID: jlID(3), Status: "unknown", Reason: "dns_error", FailureOrigin: "remote"},
		{Ordinal: 3, ItemID: jlID(7), Status: "invalid", Reason: "no_mx"},
	}
	res := func(n int, origin, state, reason, decision, unit, rule string) jlResult {
		return jlResult{Ref: jlID(n), Ordinal: n, Email: fmt.Sprintf("row%d@example.test", n), Origin: origin, State: state, Reason: reason, Decision: decision, Unit: unit, Rule: rule}
	}
	results := []jlResult{
		res(1, "module", "valid", "accepted", "charge", "m1_own", "M1-03"),
		res(2, "module", "unknown", "dns_error", "free", "", "M1-21"),
		res(3, "module", "unknown", "dns_error", "charge", "m1_own", "M1-20"),
		res(4, "platform", "malformed", "no_at", "free", "", "P-08"),
		res(5, "platform", "duplicate", "", "free", "", "P-07"),
		res(6, "platform", "suppressed", "", "free", "", "P-SUP"),
		res(7, "module", "invalid", "no_mx", "charge", "m1_own", "M1-05"),
	}
	ledger := []m1bCharge{
		{ItemRef: jlID(1), Unit: "m1_own", Cost: 1, Units: 1},
		{ItemRef: jlID(3), Unit: "m1_own", Cost: 1, Units: 1},
		{ItemRef: jlID(7), Unit: "m1_own", Cost: 1, Units: 1},
	}
	return jlM1View{Rows: rows, Results: results, Ledger: ledger, Prices: map[string]int{"m1_own": 1, "m1_cache": 1}, Report: report}
}

func (v jlM1View) clone() jlM1View {
	w := v
	w.Rows = append([]jlRow(nil), v.Rows...)
	w.Results = append([]jlResult(nil), v.Results...)
	w.Ledger = append([]m1bCharge(nil), v.Ledger...)
	w.Report = append([]m1ad.Row(nil), v.Report...)
	return w
}

// jlWantProblem requires the problems to be non-empty and to say mention.
func jlWantProblem(t *testing.T, name, mention string, problems []string) {
	t.Helper()
	if len(problems) == 0 {
		t.Errorf("%s: the comparison found nothing wrong with the tampered copy", name)
		return
	}
	if !strings.Contains(strings.Join(problems, "\n"), mention) {
		t.Errorf("%s: the comparison is red but does not say %q:\n  %s", name, mention, strings.Join(problems, "\n  "))
	}
}

// TestJobsLive_ComparisonCanFail_M1: jlM1ThreeWay accepts a consistent module 1 job and reports each of nineteen ways it can be wrong.
func TestJobsLive_ComparisonCanFail_M1(t *testing.T) {
	base := jlM1Synthetic()
	if p := jlM1ThreeWay(base); len(p) != 0 {
		t.Fatalf("control: the consistent view must pass, got %v", p)
	}
	cases := []struct {
		name, mention string
		mutate        func(v *jlM1View)
	}{
		{"a result's verdict differs from module 1's", "module 1's report says valid/accepted", func(v *jlM1View) { v.Results[0].State = "invalid" }},
		{"a result's reason differs from module 1's", "the result says valid/-", func(v *jlM1View) { v.Results[0].Reason = "" }},
		{"a charged result is called free", "the hand-written oracle says", func(v *jlM1View) { v.Results[0].Decision = "free"; v.Results[0].Unit = "" }},
		{"a free result is called a charge", "the hand-written oracle says", func(v *jlM1View) { v.Results[1].Decision = "charge"; v.Results[1].Unit = "m1_own" }},
		{"a result names another unit", "the result names unit", func(v *jlM1View) { v.Results[0].Unit = "m1_cache" }},
		{"a result with an answer the oracle cannot classify is called a charge", "the hand-written oracle says", func(v *jlM1View) {
			v.Report[2].FailureOrigin = "" // an unknown with no origin: the module broke its contract, the table says unmapped
		}},
		{"a row has no result", "has 0 result(s)", func(v *jlM1View) { v.Results = v.Results[1:] }},
		{"a row has two results", "has 2 result(s)", func(v *jlM1View) { v.Results = append(v.Results, v.Results[0]) }},
		{"a result belongs to no row", "belong to a row the job does not have", func(v *jlM1View) {
			v.Results = append(v.Results, jlResult{Ref: jlID(99), Origin: "module", State: "valid", Decision: "free"})
		}},
		{"a platform row's result is claimed by the module", "want platform", func(v *jlM1View) { v.Results[3].Origin = "module" }},
		{"a platform row's result is charged", "want free and no unit", func(v *jlM1View) { v.Results[4].Decision = "charge"; v.Results[4].Unit = "m1_own" }},
		{"a platform row's result carries another rule", "under rule", func(v *jlM1View) { v.Results[5].Rule = "P-08" }},
		{"a pending row is missing from module 1's report", "module 1's report holds 0 row(s)", func(v *jlM1View) { v.Report = v.Report[1:] }},
		{"module 1's report holds a row the platform never sent", "the platform ended it and never sent it", func(v *jlM1View) {
			v.Report = append(v.Report, m1ad.Row{Ordinal: 4, ItemID: jlID(4), Status: "invalid", Reason: "invalid_syntax"})
		}},
		{"module 1's report holds a row whose item_id is no row", "whose item_id is no row of the job", func(v *jlM1View) {
			v.Report = append(v.Report, m1ad.Row{Ordinal: 9, ItemID: jlID(98), Status: "valid", Reason: "accepted"})
		}},
		{"a charge of the ledger is missing", "holds 0 charge row(s)", func(v *jlM1View) { v.Ledger = v.Ledger[1:] }},
		{"a charge of the ledger names another unit", "the report implies unit m1_own", func(v *jlM1View) { v.Ledger[0].Unit = "m1_cache" }},
		{"a free item is charged in the ledger", "but the ledger charged it", func(v *jlM1View) {
			v.Ledger = append(v.Ledger, m1bCharge{ItemRef: jlID(2), Unit: "m1_own", Cost: 1, Units: 1})
		}},
		{"an item is charged twice", "holds 2 charge row(s)", func(v *jlM1View) { v.Ledger = append(v.Ledger, v.Ledger[0]) }},
	}
	for _, c := range cases {
		w := base.clone()
		c.mutate(&w)
		jlWantProblem(t, c.name, c.mention, jlM1ThreeWay(w))
	}
	if len(cases) != 19 {
		t.Fatalf("this control lists %d corruptions, the doc comment says nineteen (a case was added or removed without it)", len(cases))
	}
}

// jlM3Synthetic is a consistent view of a module 3 job: five pending rows (three sent, one bounced, one cancelled), a malformed row and a repeat of the first row.
func jlM3Synthetic() jlM3View {
	addr := func(n int) string { return fmt.Sprintf("m3a-ctl-%d@example.test", n) }
	row := func(n int, state string) jlRow {
		return jlRow{ID: jlID(n), Ordinal: n, State: state, Norm: addr(n), Submitted: state == "pending"}
	}
	dup := row(6, "duplicate")
	dup.Norm = addr(1)
	rows := []jlRow{row(1, "pending"), row(2, "pending"), row(3, "pending"), row(4, "pending"), row(5, "pending"), dup, {ID: jlID(7), Ordinal: 7, State: "malformed", Reason: "no_at"}}
	states := map[int]string{1: "sent", 2: "sent", 3: "bounced", 4: "cancelled", 5: "sent"}
	var report []sendapi.Item
	own := map[string]string{}
	var results []jlResult
	counts := map[string]int{}
	rcpt := map[string]m3hRcpt{}
	for n := 1; n <= 5; n++ {
		report = append(report, sendapi.Item{JobItemID: jlID(n), State: states[n], Class: sendapi.ClassTerminal})
		own[jlID(n)] = states[n]
		results = append(results, jlResult{Ref: jlID(n), Ordinal: n, Email: addr(n), Origin: "module", State: states[n], Decision: "free", Rule: jlM3Rule(states[n])})
		switch states[n] {
		case "sent":
			counts[addr(n)] = 1
			rcpt[addr(n)] = m3hRcpt{Accepted: 1}
		case "bounced":
			rcpt[addr(n)] = m3hRcpt{Rejected: 1}
		}
	}
	results = append(results,
		jlResult{Ref: jlID(6), Ordinal: 6, Email: addr(1), Origin: "platform", State: "duplicate", Decision: "free", Rule: "P-07"},
		jlResult{Ref: jlID(7), Ordinal: 7, Email: "no-at-sign", Origin: "platform", State: "malformed", Reason: "no_at", Decision: "free", Rule: "P-08"})
	return jlM3View{Rows: rows, Results: results, Report: report, Jobs: own, Counts: counts, Rcpt: rcpt}
}

func (v jlM3View) clone() jlM3View {
	w := v
	w.Rows = append([]jlRow(nil), v.Rows...)
	w.Results = append([]jlResult(nil), v.Results...)
	w.Report = append([]sendapi.Item(nil), v.Report...)
	w.Jobs = map[string]string{}
	for k, x := range v.Jobs {
		w.Jobs[k] = x
	}
	w.Counts = map[string]int{}
	for k, x := range v.Counts {
		w.Counts[k] = x
	}
	w.Rcpt = map[string]m3hRcpt{}
	for k, x := range v.Rcpt {
		w.Rcpt[k] = x
	}
	return w
}

// TestJobsLive_ComparisonCanFail_M3: jlM3ThreeWay accepts a consistent module 3 job and reports each of twenty ways it can be wrong.
func TestJobsLive_ComparisonCanFail_M3(t *testing.T) {
	base := jlM3Synthetic()
	if p := jlM3ThreeWay(base); len(p) != 0 {
		t.Fatalf("control: the consistent view must pass, got %v", p)
	}
	a := func(n int) string { return fmt.Sprintf("m3a-ctl-%d@example.test", n) }
	cases := []struct {
		name, mention string
		mutate        func(v *jlM3View)
	}{
		{"a result's state differs from the report's", "module 3's report says sent", func(v *jlM3View) { v.Results[0].State = "bounced" }},
		{"a result of the module is charged", "want free and no unit", func(v *jlM3View) { v.Results[0].Decision = "charge" }},
		{"a result names a unit", "want free and no unit", func(v *jlM3View) { v.Results[0].Unit = "m2_accepted" }},
		{"a cancelled item carries the wildcard rule", "the table says", func(v *jlM3View) { v.Results[3].Rule = "M3-99" }},
		{"a result is missing", "has 0 result(s)", func(v *jlM3View) { v.Results = v.Results[1:] }},
		{"a platform row's result is the module's", "want platform", func(v *jlM3View) { v.Results[5].Origin = "module" }},
		{"a sent item has two messages at the sink", "the sink holds 2 message(s)", func(v *jlM3View) { v.Counts[a(1)] = 2 }},
		{"a sent item has no message at the sink", "the sink holds 0 message(s)", func(v *jlM3View) { v.Counts[a(2)] = 0 }},
		{"a sent item was never accepted at RCPT", "want one message and one acceptance", func(v *jlM3View) { v.Rcpt[a(2)] = m3hRcpt{} }},
		{"a bounced item has a message", "want no message and one rejection", func(v *jlM3View) { v.Counts[a(3)] = 1 }},
		{"a bounced item was rejected twice", "want no message and one rejection", func(v *jlM3View) { v.Rcpt[a(3)] = m3hRcpt{Rejected: 2} }},
		{"a cancelled item reached the sink", "want nothing at all", func(v *jlM3View) { v.Counts[a(4)] = 1 }},
		{"an item of the report is missing", "module 3's report holds 0 item(s)", func(v *jlM3View) { v.Report = v.Report[1:] }},
		{"an item of the report is not final", "which is not final", func(v *jlM3View) { v.Report[0].State = "sending"; v.Report[0].Class = sendapi.ClassInFlight }},
		{"module 3's own table disagrees with its report", "module 3's own job table says", func(v *jlM3View) { v.Jobs[jlID(3)] = "sent" }},
		{"a repeat is in module 3's report", "the platform ended it and never ran it", func(v *jlM3View) {
			v.Report = append(v.Report, sendapi.Item{JobItemID: jlID(6), State: "sent", Class: sendapi.ClassTerminal})
		}},
		{"a malformed row has a job at module 3", "it never reached the module", func(v *jlM3View) { v.Jobs[jlID(7)] = "queued" }},
		{"a message to an address no pending row owns", "no pending row of the job owns", func(v *jlM3View) { v.Counts["m3a-ctl-nobody@example.test"] = 1 }},
		{"a RCPT answer for an address no pending row owns", "answered RCPT", func(v *jlM3View) { v.Rcpt["m3a-ctl-nobody@example.test"] = m3hRcpt{Accepted: 1} }},
		{"the ledger holds a row of the job", "module 3 is not metered", func(v *jlM3View) { v.Ledger = 1 }},
	}
	for _, c := range cases {
		w := base.clone()
		c.mutate(&w)
		jlWantProblem(t, c.name, c.mention, jlM3ThreeWay(w))
	}
	if len(cases) != 20 {
		t.Fatalf("this control lists %d corruptions, the doc comment says twenty (a case was added or removed without it)", len(cases))
	}
}

// TestJobsLive_ComparisonCanFail_Chain: the owner's selection rule, the chain's rows against their source, and the sink's delta each report what is wrong with them.
func TestJobsLive_ComparisonCanFail_Chain(t *testing.T) {
	for _, c := range []struct {
		verdict string
		include bool
		want    bool
	}{
		{"valid", false, true}, {"risky", false, true}, {"invalid", false, false}, {"unknown", false, false}, {"unknown", true, true}, {"invalid", true, false}, {"", true, false}, {"bounced", true, false},
	} {
		if got := jlSelects(c.verdict, c.include); got != c.want {
			t.Errorf("jlSelects(%q, include %v) = %v, want %v", c.verdict, c.include, got, c.want)
		}
	}

	src := []jlSrc{{jlID(1), "a1@example.test", "valid"}, {jlID(2), "a2@example.test", "risky"}, {jlID(3), "a3@example.test", "invalid"}, {jlID(4), "a4@example.test", "unknown"}, {jlID(5), "a5@example.test", "unknown"}}
	chain := func(include bool) []jlRow {
		var out []jlRow
		for i, s := range src {
			r := jlRow{ID: jlID(100 + i), Ordinal: i + 1, Norm: s.Email, Source: s.Ref, State: "excluded", Reason: s.State}
			if jlSelects(s.State, include) {
				r.State, r.Reason = "pending", ""
			}
			out = append(out, r)
		}
		return out
	}
	for _, include := range []bool{false, true} {
		if p := jlChainRowsProblems(src, chain(include), include); len(p) != 0 {
			t.Fatalf("control: the consistent chain (unknown included: %v) must pass, got %v", include, p)
		}
	}
	// the chain of the default choice judged by the choice to include unknown, and the reverse
	jlWantProblem(t, "the default chain judged as if unknown were included", "became a chain row in state", jlChainRowsProblems(src, chain(false), true))
	jlWantProblem(t, "the including chain judged as if unknown were excluded", "became a chain row in state", jlChainRowsProblems(src, chain(true), false))
	mut := []struct {
		name, mention string
		rows          func() []jlRow
	}{
		{"an invalid verdict is sent to", "became a chain row in state", func() []jlRow { r := chain(false); r[2].State, r[2].Reason = "pending", ""; return r }},
		{"a valid verdict is left out", "became a chain row in state", func() []jlRow { r := chain(false); r[0].State, r[0].Reason = "excluded", "valid"; return r }},
		{"an excluded row names another verdict", "says the reason", func() []jlRow { r := chain(false); r[2].Reason = "valid"; return r }},
		{"a row of the source is missing", "for 5 module result(s)", func() []jlRow { return chain(false)[1:] }},
		{"a row names no source item", "no row of the chain comes from the source item", func() []jlRow { r := chain(false); r[0].Source = jlID(77); return r }},
		{"a row is another address than its source's", "is not the address of the source result", func() []jlRow { r := chain(false); r[1].Norm = "other@example.test"; return r }},
	}
	for _, m := range mut {
		jlWantProblem(t, m.name, m.mention, jlChainRowsProblems(src, m.rows(), false))
	}

	// the sink's delta
	universe := []string{"a1@example.test", "a2@example.test", "a3@example.test", "a4@example.test", "a5@example.test"}
	sel := jlSelectedSet(src, false)
	if len(sel) != 2 || !sel["a1@example.test"] || !sel["a2@example.test"] {
		t.Fatalf("the default selection of the synthetic source is %v, want a1 and a2", sel)
	}
	before := map[string]int{"a1@example.test": 1}
	good := map[string]int{"a1@example.test": 2, "a2@example.test": 1}
	if p := jlSinkDeltaProblems(before, good, sel, universe); len(p) != 0 {
		t.Fatalf("control: the sink delta of exactly the selected addresses must pass, got %v", p)
	}
	jlWantProblem(t, "an excluded address reached the sink", "selected=false gained 1", jlSinkDeltaProblems(before, map[string]int{"a1@example.test": 2, "a2@example.test": 1, "a4@example.test": 1}, sel, universe))
	jlWantProblem(t, "a selected address never reached the sink", "selected=true gained 0", jlSinkDeltaProblems(before, map[string]int{"a1@example.test": 2}, sel, universe))
	jlWantProblem(t, "a selected address got two messages", "selected=true gained 2", jlSinkDeltaProblems(before, map[string]int{"a1@example.test": 3, "a2@example.test": 2}, sel, universe))
	// the control of the sink check as the live chain test uses it: what the INCLUDING choice delivers is what the DEFAULT choice's check refuses
	all := jlSelectedSet(src, true)
	delivered := map[string]int{"a1@example.test": 1, "a2@example.test": 1, "a4@example.test": 1, "a5@example.test": 1}
	if p := jlSinkDeltaProblems(map[string]int{}, delivered, all, universe); len(p) != 0 {
		t.Fatalf("control: the sink delta of the including choice must pass for the including selection, got %v", p)
	}
	jlWantProblem(t, "the default check on what the including choice delivered", "selected=false gained 1", jlSinkDeltaProblems(map[string]int{}, delivered, sel, universe))
}

// TestJobsLive_ComparisonCanFail_Lists: the lists the live tests send are what they claim, by the platform's own address rule (norm.Validate, the function the store classifies with): exactly the pending
// addresses, the malformed rows and the repeats the doc comments say, with a repeat always AFTER the row it repeats; and the control is a list that classifies otherwise.
func TestJobsLive_ComparisonCanFail_Lists(t *testing.T) {
	classify := func(rows []string) (pending, malformed, duplicates int) {
		seen := map[string]bool{}
		for _, raw := range rows {
			n, err := norm.Validate(raw)
			switch {
			case err != nil:
				malformed++
			case seen[n]:
				duplicates++
			default:
				seen[n] = true
				pending++
			}
		}
		return pending, malformed, duplicates
	}
	texts := func(l jlList) []string {
		out := make([]string, len(l.Rows))
		for i, r := range l.Rows {
			out[i] = r.Email
		}
		return out
	}
	for _, c := range []struct{ pending, domains, malformed, repeats int }{{32, 4, 4, 4}, {12, 3, 2, 1}, {10, 2, 0, 0}, {6, 2, 1, 1}, {5, 1, 3, 4}} {
		l := jlBuildList(jlAddrs("ctl", c.pending, c.domains), c.malformed, c.repeats)
		p, m, d := classify(texts(l))
		if p != c.pending || m != c.malformed || d != c.repeats || len(l.Rows) != c.pending+c.malformed+c.repeats {
			t.Errorf("a list of %d pending, %d malformed and %d repeats (%d rows) classifies as %d pending, %d malformed, %d repeats: the live tests' counts would be wrong", c.pending, c.malformed, c.repeats, len(l.Rows), p, m, d)
		}
	}
	// the classifier itself, on a hand-made list: the repeat that comes AFTER its original is the repeat, a text that is not an address is malformed
	if p, m, d := classify([]string{"b@x.example.test", "A@x.example.test", "not an address", "a@x.example.test"}); p != 2 || m != 1 || d != 1 {
		t.Errorf("the classifier: the hand-made list classifies as %d pending, %d malformed, %d repeats, want 2, 1, 1", p, m, d)
	}
	addrs := jlAddrs("ctl", 32, 4)
	seen := map[string]bool{}
	for _, a := range addrs {
		if seen[a] {
			t.Fatalf("jlAddrs repeats an address")
		}
		seen[a] = true
		if _, err := norm.Validate(a); err != nil {
			t.Fatalf("jlAddrs made an address the platform's rule refuses: %v", err)
		}
	}
}
