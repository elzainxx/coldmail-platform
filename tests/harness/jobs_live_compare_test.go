//go:build harness

package harness

import (
	"fmt"
	"sort"
	"strings"

	m1ad "github.com/coldmail-platform/platform/internal/adapters/m1"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
)

// THE COMPARISONS OF THE LIVE JOB TESTS (TestJobsLive_*). Pure functions: they take what the test READ (the platform's rows, results and ledger, the module's own report, the sink's counters) and return
// every disagreement as a sentence, never failing a test themselves, so that the SAME function can be run on a corrupted copy of what it just accepted and be shown to report the corruption
// (TestJobsLive_ComparisonCanFail_*). A comparison that cannot fail proves nothing.
//
// THE THREE WAYS (Docs/phase-4-spec.md Locked 25): the ledger's charge rows, platform.job_results, and the MODULE'S OWN REPORT (module 1: its job and results listing, read with the user's derived key; module 3:
// its campaign report, its own job table and the SMTP sink's counters per recipient), item by item. The oracles are written out BY HAND from internal/charge/rules.psv and the owner's rulings (m1bOracle for
// module 1, jlM3Rule below for module 3); neither calls charge.Decide, which is what the settlement itself used.
//
// NO MESSAGE CARRIES AN ADDRESS: a row is named by its ordinal and its state (the log of a harness run is public).

// jlRow is a row of platform.job_rows as the tests read it.
type jlRow struct {
	ID        string
	Ordinal   int
	Norm      string // email_norm, "" for a malformed row
	State     string // pending, malformed, duplicate, suppressed, excluded
	Reason    string
	Source    string // job_rows.source_item: the source job's item_ref of a chained send's row, "" otherwise
	Submitted bool   // submitted_at is stamped
}

// jlResult is a row of platform.job_results as the tests read it.
type jlResult struct {
	Ref      string
	Ordinal  int
	Email    string
	Origin   string // module or platform
	State    string
	Reason   string
	Decision string // charge, free or unmapped
	Unit     string
	Rule     string
}

// jlPlatformRule is the rule label of the platform's own result of a row in a state, written out by hand from internal/charge/rules.psv and the two labels the store owns (P-SUP, P-EXC).
func jlPlatformRule(state string) string {
	switch state {
	case "malformed":
		return "P-08"
	case "duplicate":
		return "P-07"
	case "suppressed":
		return "P-SUP"
	case "excluded":
		return "P-EXC"
	case "cancelled":
		return "P-01"
	case "expired":
		return "P-02"
	case "infrastructure_failure":
		return "P-04"
	}
	return "?"
}

// jlM3Rule is the rule label of a module 3 result, from the table: M3-01 for a cancelled item, M3-99 (module 3 is not metered) for every other state.
func jlM3Rule(state string) string {
	if state == "cancelled" {
		return "M3-01"
	}
	return "M3-99"
}

// jlByRef groups results by the row they belong to.
func jlByRef(rs []jlResult) map[string][]jlResult {
	out := map[string][]jlResult{}
	for _, r := range rs {
		out[r.Ref] = append(out[r.Ref], r)
	}
	return out
}

// jlPlatformResultProblems checks the platform's own result of one row that never reached a module: the origin, the state, free, no unit, the rule.
func jlPlatformResultProblems(row jlRow, res jlResult) []string {
	var p []string
	if res.Origin != "platform" {
		p = append(p, fmt.Sprintf("row %d (%s) has a result of origin %q, want platform: the platform ended it, no module did", row.Ordinal, row.State, res.Origin))
	}
	if res.State != row.State {
		p = append(p, fmt.Sprintf("row %d (%s) has a result in state %q, want %q", row.Ordinal, row.State, res.State, row.State))
	}
	if res.Decision != "free" || res.Unit != "" {
		p = append(p, fmt.Sprintf("row %d (%s) has a result with decision %q unit %q, want free and no unit: the platform's own outcomes are never charged", row.Ordinal, row.State, res.Decision, res.Unit))
	}
	if want := jlPlatformRule(row.State); res.Rule != want {
		p = append(p, fmt.Sprintf("row %d (%s) has a result under rule %q, want %q", row.Ordinal, row.State, res.Rule, want))
	}
	if row.State == "excluded" && res.Reason != row.Reason {
		p = append(p, fmt.Sprintf("row %d is excluded for %q but its result says %q", row.Ordinal, row.Reason, res.Reason))
	}
	return p
}

// ---- module 1 ----------------------------------------------------------------------------------------------------------------------------------------------------------------

// jlM1View is everything one module 1 job leaves behind, read from the three places.
type jlM1View struct {
	Rows    []jlRow
	Results []jlResult
	Ledger  []m1bCharge
	Prices  map[string]int // the job's frozen price snapshot
	Report  []m1ad.Row     // module 1's own listing, read with the user's key
}

// jlM1ThreeWay: for every row of the job, ONE result; a pending row is in module 1's report exactly once, its result carries the report's verdict and the decision the hand-written oracle (m1bOracle) makes of it, and
// the ledger holds a charge under its id exactly when the oracle says charge, of the oracle's unit at the frozen price (m1bCompare); a row of the platform's own (malformed, duplicate, suppressed) has the platform's
// result, is free, is in no report and has no charge; and nothing in the results, the report or the ledger names a row the job does not have.
func jlM1ThreeWay(v jlM1View) []string {
	var p []string
	say := func(f string, a ...any) { p = append(p, fmt.Sprintf(f, a...)) }
	res := jlByRef(v.Results)
	rep := map[string][]m1ad.Row{}
	for _, r := range v.Report {
		rep[r.ItemID] = append(rep[r.ItemID], r)
	}
	known := map[string]jlRow{}
	for _, row := range v.Rows {
		known[row.ID] = row
		rs := res[row.ID]
		if len(rs) != 1 {
			say("row %d (%s) has %d result(s), want exactly 1", row.Ordinal, row.State, len(rs))
			continue
		}
		r := rs[0]
		reps := rep[row.ID]
		if row.State != "pending" {
			p = append(p, jlPlatformResultProblems(row, r)...)
			if len(reps) != 0 {
				say("module 1's report holds %d row(s) for row %d (%s): the platform ended it and never sent it", len(reps), row.Ordinal, row.State)
			}
			continue
		}
		if len(reps) != 1 {
			say("row %d is pending and module 1's report holds %d row(s) for it, want exactly 1", row.Ordinal, len(reps))
			continue
		}
		m := reps[0]
		d := m1bOracle(m)
		if r.Origin != "module" {
			say("row %d has a result of origin %q, want module (module 1 answered it)", row.Ordinal, r.Origin)
		}
		if r.State != m.Status || r.Reason != m.Reason {
			say("row %d: the result says %s/%s, module 1's report says %s/%s", row.Ordinal, r.State, orDash(r.Reason), m.Status, orDash(m.Reason))
		}
		if r.Decision != d.Kind {
			say("row %d: the result says %q, the hand-written oracle says %q for module 1's %s/%s (%s)", row.Ordinal, r.Decision, d.Kind, m.Status, m.Reason, d.Why)
		}
		wantUnit := ""
		if d.Kind == "charge" {
			wantUnit = d.Unit
		}
		if r.Unit != wantUnit {
			say("row %d: the result names unit %q, the oracle says %q", row.Ordinal, r.Unit, wantUnit)
		}
	}
	for ref, rs := range res {
		if _, ok := known[ref]; !ok {
			say("%d result(s) belong to a row the job does not have", len(rs))
		}
	}
	for id, ms := range rep {
		if _, ok := known[id]; !ok {
			say("module 1's report holds %d row(s) whose item_id is no row of the job", len(ms))
		}
	}
	p = append(p, m1bCompare(v.Report, v.Ledger, v.Prices)...)
	sort.Strings(p)
	return p
}

// ---- module 3 ----------------------------------------------------------------------------------------------------------------------------------------------------------------

// jlM3View is everything one module 3 job leaves behind.
type jlM3View struct {
	Rows    []jlRow
	Results []jlResult
	Report  []sendapi.Item     // module 3's campaign report, every page
	Jobs    map[string]string  // module 3's own job table: job_item_id -> state
	Counts  map[string]int     // messages the sink holds, per exact recipient address (this test's prefix only)
	Rcpt    map[string]m3hRcpt // what the sink answered at RCPT TO, per address
	Ledger  int                // ledger rows of the job (module 3 is not metered: none)
}

// jlM3ThreeWay: the pending rows are the module's items exactly (report, module 3's own job table, public.job_items are checked by the caller), each result carries the report's state, is free, names no unit and is
// labelled by the table's row (M3-01 for cancelled, M3-99 otherwise); the sink holds ONE message and one accepted RCPT for a sent item, no message and one rejected RCPT for a bounced one, and nothing at all for a cancelled
// or failed one; a row of the platform's own has the platform's result, is not in the report, not in module 3's table; no address of this test is at the sink more often than its pending rows say; and the ledger holds nothing.
func jlM3ThreeWay(v jlM3View) []string {
	var p []string
	say := func(f string, a ...any) { p = append(p, fmt.Sprintf(f, a...)) }
	res := jlByRef(v.Results)
	rep := map[string][]sendapi.Item{}
	for _, it := range v.Report {
		rep[it.JobItemID] = append(rep[it.JobItemID], it)
	}
	known := map[string]jlRow{}
	owner := map[string][]jlRow{} // address -> the pending rows that own it
	for _, row := range v.Rows {
		known[row.ID] = row
		if row.State == "pending" {
			owner[row.Norm] = append(owner[row.Norm], row)
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
				say("module 3's report holds an item for row %d (%s): the platform ended it and never ran it", row.Ordinal, row.State)
			}
			if _, at := v.Jobs[row.ID]; at {
				say("module 3 holds a job for row %d (%s): it never reached the module", row.Ordinal, row.State)
			}
			continue
		}
		items := rep[row.ID]
		if len(items) != 1 {
			say("row %d is pending and module 3's report holds %d item(s) for it, want exactly 1", row.Ordinal, len(items))
			continue
		}
		it := items[0]
		if it.Class != sendapi.ClassTerminal {
			say("row %d is reported in state %s (class %s), which is not final", row.Ordinal, it.State, it.Class)
		}
		if st, ok := v.Jobs[row.ID]; !ok || st != it.State {
			say("row %d: module 3's own job table says %q (present %v), its report says %q", row.Ordinal, st, ok, it.State)
		}
		if r.Origin != "module" || r.State != it.State {
			say("row %d: the result says %s from %s, module 3's report says %s", row.Ordinal, r.State, r.Origin, it.State)
		}
		if r.Decision != "free" || r.Unit != "" {
			say("row %d: the result says decision %q unit %q, want free and no unit (module 3 is not metered)", row.Ordinal, r.Decision, r.Unit)
		}
		if want := jlM3Rule(it.State); r.Rule != want {
			say("row %d: the result is under rule %q, the table says %q for a %s item", row.Ordinal, r.Rule, want, it.State)
		}
		c, rc := v.Counts[row.Norm], v.Rcpt[row.Norm]
		switch it.State {
		case "sent":
			if c != 1 || rc.Accepted != 1 || rc.Rejected != 0 {
				say("row %d is sent: the sink holds %d message(s) and answered RCPT accepted %d rejected %d, want one message and one acceptance", row.Ordinal, c, rc.Accepted, rc.Rejected)
			}
		case "bounced":
			if c != 0 || rc.Rejected != 1 || rc.Accepted != 0 {
				say("row %d is bounced: the sink holds %d message(s) and answered RCPT accepted %d rejected %d, want no message and one rejection", row.Ordinal, c, rc.Accepted, rc.Rejected)
			}
		case "cancelled", "failed":
			if c != 0 || rc != (m3hRcpt{}) {
				say("row %d is %s: the sink holds %d message(s) and answered RCPT %+v, want nothing at all", row.Ordinal, it.State, c, rc)
			}
		default:
			say("row %d is in state %q, which a finished job cannot hold", row.Ordinal, it.State)
		}
	}
	for ref, rs := range res {
		if _, ok := known[ref]; !ok {
			say("%d result(s) belong to a row the job does not have", len(rs))
		}
	}
	for id := range rep {
		if _, ok := known[id]; !ok {
			say("module 3's report holds an item that is no row of the job")
		}
	}
	for id := range v.Jobs {
		if _, ok := known[id]; !ok {
			say("module 3's own table holds a job that is no row of the job")
		}
	}
	for a, n := range v.Counts {
		if _, ok := owner[a]; !ok && n != 0 {
			say("the sink holds %d message(s) to an address that no pending row of the job owns", n)
		}
		if len(owner[a]) > 1 {
			say("two pending rows own one address: the platform's duplicate rule did not hold")
		}
	}
	for a, rc := range v.Rcpt {
		if _, ok := owner[a]; !ok && rc != (m3hRcpt{}) {
			say("the sink answered RCPT %+v for an address that no pending row of the job owns", rc)
		}
	}
	if v.Ledger != 0 {
		say("the ledger holds %d row(s) of the job: module 3 is not metered, nothing is reserved or charged", v.Ledger)
	}
	sort.Strings(p)
	return p
}

// ---- the chain ---------------------------------------------------------------------------------------------------------------------------------------------------------------

// jlSelects is the owner's rule for a chained send, written out by hand (not the store's selects): valid and risky are sent to, unknown only when the user chose to include it, invalid and anything else never.
func jlSelects(verdict string, includeUnknown bool) bool {
	switch verdict {
	case "valid", "risky":
		return true
	case "unknown":
		return includeUnknown
	}
	return false
}

// jlSrc is one module result of the source verify job.
type jlSrc struct {
	Ref   string
	Email string
	State string
}

// jlChainRowsProblems: the chain's rows are the source's module results one for one, in order; a row is pending exactly when the owner's rule selects its verdict, and an excluded row says the verdict it was
// excluded for; every row names the source item it came from.
func jlChainRowsProblems(src []jlSrc, rows []jlRow, includeUnknown bool) []string {
	var p []string
	if len(rows) != len(src) {
		p = append(p, fmt.Sprintf("the chain has %d row(s) for %d module result(s) of its source", len(rows), len(src)))
	}
	by := map[string]jlRow{}
	for _, r := range rows {
		by[r.Source] = r
	}
	for _, s := range src {
		r, ok := by[s.Ref]
		if !ok {
			p = append(p, fmt.Sprintf("no row of the chain comes from the source item of a %s result", s.State))
			continue
		}
		want := "excluded"
		if jlSelects(s.State, includeUnknown) {
			want = "pending"
		}
		if r.State != want {
			p = append(p, fmt.Sprintf("a source result of verdict %s became a chain row in state %q, the owner's rule says %q (unknown included: %v)", s.State, r.State, want, includeUnknown))
		}
		if want == "excluded" && r.Reason != s.State {
			p = append(p, fmt.Sprintf("a row excluded for verdict %s says the reason %q", s.State, r.Reason))
		}
		if r.Norm != s.Email {
			p = append(p, fmt.Sprintf("chain row %d is not the address of the source result it names", r.Ordinal))
		}
	}
	sort.Strings(p)
	return p
}

// jlSinkDeltaProblems: for every address of the source list, what the sink gained during a chained job is exactly what the selection says: one message to a selected address, none to any other. before and
// after are the sink's message counts per exact address.
func jlSinkDeltaProblems(before, after map[string]int, selected map[string]bool, universe []string) []string {
	var p []string
	for _, a := range universe {
		want := 0
		if selected[a] {
			want = 1
		}
		if got := after[a] - before[a]; got != want {
			p = append(p, fmt.Sprintf("an address that is selected=%v gained %d message(s) at the sink, want %d", selected[a], got, want))
		}
	}
	return p
}

// jlSelectedSet is the set of addresses the owner's rule selects from a source.
func jlSelectedSet(src []jlSrc, includeUnknown bool) map[string]bool {
	out := map[string]bool{}
	for _, s := range src {
		if jlSelects(s.State, includeUnknown) {
			out[s.Email] = true
		}
	}
	return out
}

func jlStates(src []jlSrc) string {
	n := map[string]int{}
	for _, s := range src {
		n[s.State]++
	}
	keys := make([]string, 0, len(n))
	for k := range n {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s=%d", k, n[k])
	}
	return strings.Join(parts, " ")
}
