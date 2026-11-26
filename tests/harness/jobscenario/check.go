//go:build harness

package main

import (
	"fmt"
	"sort"
	"strings"

	m1 "github.com/coldmail-platform/platform/internal/adapters/m1"
	"github.com/coldmail-platform/platform/internal/charge"
)

// The invariants of a finished job as PURE functions over what the database and module 1 said: each returns the problems it found (none is a pass). They are pure so that verify's own control can
// feed each of them a corrupted copy of a consistent fixture and require a problem (controlDetectsCorruption): a check that cannot go red proves nothing.

// rowInfo is one row of platform.job_rows (Email is what the row's result must name: the normalised address, or the raw text of a row that did not normalise).
type rowInfo struct {
	ID      string
	Ordinal int
	State   string
	Email   string
}

// resultInfo is one row of platform.job_results.
type resultInfo struct {
	Ref, Email, Origin, State, Reason, Decision, Unit, Rule string
	Ordinal                                                 int
}

// chargeInfo is one `charge` row of the ledger of the job.
type chargeInfo struct {
	Ref, Unit string
	Units     int
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// sample is at most five of the strings, for a message.
func sample(s []string) string {
	sort.Strings(s)
	if len(s) > 5 {
		return strings.Join(s[:5], ", ") + fmt.Sprintf(", ... (%d in all)", len(s))
	}
	return strings.Join(s, ", ")
}

// ---- (a) the rows and their results ------------------------------------------------------------------------------------------------------------------------------------------------

// rowsEqualResults: a job holds exactly as many results as rows.
func rowsEqualResults(rows []rowInfo, results []resultInfo) []string {
	if len(rows) != len(results) {
		return []string{fmt.Sprintf("the job has %s and %s", plural(len(rows), "row"), plural(len(results), "result"))}
	}
	return nil
}

// everyPendingRowHasOneResult: every row has exactly one result, a pending row's from the module and any other row's from the platform with the row's own state.
func everyPendingRowHasOneResult(rows []rowInfo, results []resultInfo) []string {
	byRef := map[string][]resultInfo{}
	for _, r := range results {
		byRef[r.Ref] = append(byRef[r.Ref], r)
	}
	var missing, many, wrong []string
	known := map[string]bool{}
	for _, w := range rows {
		known[w.ID] = true
		rs := byRef[w.ID]
		switch {
		case len(rs) == 0:
			missing = append(missing, fmt.Sprintf("%s(ordinal %d, %s)", w.ID[:8], w.Ordinal, w.State))
		case len(rs) > 1:
			many = append(many, w.ID[:8])
		case w.State == "pending" && rs[0].Origin != "module":
			wrong = append(wrong, fmt.Sprintf("%s: a pending row's result is %s's", w.ID[:8], rs[0].Origin))
		case w.State != "pending" && (rs[0].Origin != "platform" || rs[0].State != w.State):
			wrong = append(wrong, fmt.Sprintf("%s: a %s row's result is %s/%s", w.ID[:8], w.State, rs[0].Origin, rs[0].State))
		}
	}
	var stray []string
	for ref := range byRef {
		if !known[ref] {
			stray = append(stray, ref[:min(8, len(ref))])
		}
	}
	var out []string
	if len(missing) > 0 {
		out = append(out, "rows with no result: "+sample(missing))
	}
	if len(many) > 0 {
		out = append(out, "rows with more than one result: "+sample(many))
	}
	if len(wrong) > 0 {
		out = append(out, "results of the wrong origin or state: "+sample(wrong))
	}
	if len(stray) > 0 {
		out = append(out, "results of no row of the job: "+sample(stray))
	}
	return out
}

// resultsNameTheirRows: each result names the address and the position of its own row.
func resultsNameTheirRows(rows []rowInfo, results []resultInfo) []string {
	by := map[string]rowInfo{}
	for _, w := range rows {
		by[w.ID] = w
	}
	var bad []string
	for _, r := range results {
		w, ok := by[r.Ref]
		switch {
		case !ok:
			bad = append(bad, r.Ref[:min(8, len(r.Ref))]+" (no such row)")
		case r.Email != w.Email:
			bad = append(bad, fmt.Sprintf("%s names another address than its row", r.Ref[:8]))
		case r.Ordinal != w.Ordinal:
			bad = append(bad, fmt.Sprintf("%s has ordinal %d, its row %d", r.Ref[:8], r.Ordinal, w.Ordinal))
		}
	}
	if len(bad) > 0 {
		return []string{"results that do not name their row: " + sample(bad)}
	}
	return nil
}

// ---- (c) the ledger -------------------------------------------------------------------------------------------------------------------------------------------------------------------

// atMostOneChargePerItem: no item has two charge rows.
func atMostOneChargePerItem(charges []chargeInfo) []string {
	n := map[string]int{}
	for _, c := range charges {
		n[c.Ref]++
	}
	var twice []string
	for ref, k := range n {
		if k > 1 {
			twice = append(twice, fmt.Sprintf("%s x%d", ref[:min(8, len(ref))], k))
		}
	}
	if len(twice) > 0 {
		return []string{"items charged more than once: " + sample(twice)}
	}
	return nil
}

// chargesEqualResultsWithAUnit: the ledger's charges and the results that name a unit are the same set, with the same unit, and a charge is one unit.
func chargesEqualResultsWithAUnit(charges []chargeInfo, results []resultInfo) []string {
	led := map[string]string{}
	var out []string
	for _, c := range charges {
		led[c.Ref] = c.Unit
		if c.Units != 1 {
			out = append(out, fmt.Sprintf("the charge of %s is for %d units, an item is one", c.Ref[:min(8, len(c.Ref))], c.Units))
		}
	}
	res := map[string]string{}
	for _, r := range results {
		if r.Unit != "" {
			res[r.Ref] = r.Unit
		}
	}
	var noRes, noCharge, other []string
	for ref, u := range led {
		ru, ok := res[ref]
		switch {
		case !ok:
			noRes = append(noRes, ref[:min(8, len(ref))])
		case ru != u:
			other = append(other, fmt.Sprintf("%s ledger %s, result %s", ref[:min(8, len(ref))], u, ru))
		}
	}
	for ref := range res {
		if _, ok := led[ref]; !ok {
			noCharge = append(noCharge, ref[:min(8, len(ref))])
		}
	}
	if len(noRes) > 0 {
		out = append(out, "ledger charges with no result naming a unit: "+sample(noRes))
	}
	if len(noCharge) > 0 {
		out = append(out, "results naming a unit with no ledger charge: "+sample(noCharge))
	}
	if len(other) > 0 {
		out = append(out, "charges whose unit differs from the result's: "+sample(other))
	}
	return out
}

// reserveFacts are the numbers of the job's reserve and the user's balance at the end.
type reserveFacts struct {
	Reserves, Releases                  int64 // ledger rows of kind reserve and release for the job
	ReleasedCredits                     int64 // the credits the release row returned (0 when there is none)
	Reserved, Charged, Released         int64 // platform.reserves
	Granted, Available, BalanceReserved int64 // the user's grants (ledger), balances.available and balances.reserved
	ChargedUnitCostSum                  int64 // sum(units * unit_cost) of the job's charge rows
}

// reserveClosed: one reserve, whose remainder is zero, and a user who holds exactly what the grant less the charges leaves (nothing is stuck reserved).
func reserveClosed(f reserveFacts) []string {
	var out []string
	if f.Reserves != 1 {
		out = append(out, fmt.Sprintf("the ledger holds %s for the job, want exactly one", plural(int(f.Reserves), "reserve row")))
	}
	if f.Reserved-f.Charged-f.Released != 0 {
		out = append(out, fmt.Sprintf("reserved %d - charged %d - released %d = %d, want 0", f.Reserved, f.Charged, f.Released, f.Reserved-f.Charged-f.Released))
	}
	if f.Charged != f.ChargedUnitCostSum {
		out = append(out, fmt.Sprintf("platform.reserves says %d credits were charged, the job's charge rows add up to %d", f.Charged, f.ChargedUnitCostSum))
	}
	if f.BalanceReserved != 0 {
		out = append(out, fmt.Sprintf("the user still holds %d reserved credits after the job ended", f.BalanceReserved))
	}
	if f.Available != f.Granted-f.ChargedUnitCostSum {
		out = append(out, fmt.Sprintf("the user holds %d credits, the grant %d less the charges %d is %d", f.Available, f.Granted, f.ChargedUnitCostSum, f.Granted-f.ChargedUnitCostSum))
	}
	return out
}

// oneReleasePerReserve: the remainder goes back exactly once (one release row when there was a remainder, none when the charges used the whole reserve).
func oneReleasePerReserve(f reserveFacts) []string {
	remainder := f.Reserved - f.Charged // what the release must return (the reserve's own `released` is what it did return)
	var out []string
	switch {
	case remainder > 0 && f.Releases != 1:
		out = append(out, fmt.Sprintf("%d credits were left in the reserve and the ledger holds %s, want exactly one", remainder, plural(int(f.Releases), "release row")))
	case remainder == 0 && f.Releases != 0:
		out = append(out, fmt.Sprintf("the charges used the whole reserve and the ledger holds %s", plural(int(f.Releases), "release row")))
	case f.Releases == 1 && f.ReleasedCredits != remainder:
		out = append(out, fmt.Sprintf("the release returned %d credits, the remainder was %d", f.ReleasedCredits, remainder))
	}
	return out
}

// ---- (d) module 1's report, the results and the ledger: three ways --------------------------------------------------------------------------------------------------------------------

// reportCovers: module 1's report names exactly the pending rows of the job, each once, and no row twice as a repeat.
func reportCovers(report []m1.Row, pending map[string]rowInfo) []string {
	seen := map[string]int{}
	var out []string
	for _, r := range report {
		seen[r.ItemID]++
		if r.DuplicateOf != nil {
			out = append(out, fmt.Sprintf("module 1 marks item %s as a repeat of ordinal %d, and the platform sends no repeat", r.ItemID[:min(8, len(r.ItemID))], *r.DuplicateOf))
		}
	}
	var missing, extra, twice []string
	for id, w := range pending {
		if seen[id] == 0 {
			missing = append(missing, fmt.Sprintf("%s(ordinal %d)", id[:8], w.Ordinal))
		}
	}
	for id, k := range seen {
		if _, ok := pending[id]; !ok {
			extra = append(extra, id[:min(8, len(id))])
		}
		if k > 1 {
			twice = append(twice, id[:min(8, len(id))])
		}
	}
	if len(missing) > 0 {
		out = append(out, "pending rows module 1's report does not name: "+sample(missing))
	}
	if len(extra) > 0 {
		out = append(out, "items module 1 reports that are no pending row: "+sample(extra))
	}
	if len(twice) > 0 {
		out = append(out, "items module 1 reports more than once: "+sample(twice))
	}
	return out
}

// reportEqualsResults: for each item of the report, the platform's result is the module's own words (state and reason) under the decision the charge table makes of them.
func reportEqualsResults(report []m1.Row, results map[string]resultInfo) []string {
	var wrong, none []string
	for _, r := range report {
		res, ok := results[r.ItemID]
		short := r.ItemID[:min(8, len(r.ItemID))]
		if !ok {
			none = append(none, short)
			continue
		}
		d := charge.Decide(r.Outcome())
		switch {
		case res.Origin != "module":
			wrong = append(wrong, fmt.Sprintf("%s: the result is %s's, the module answered it", short, res.Origin))
		case res.State != r.Status || res.Reason != r.Reason:
			wrong = append(wrong, fmt.Sprintf("%s: module 1 says %s/%s, the result says %s/%s", short, r.Status, r.Reason, res.State, res.Reason))
		case d.Kind == charge.NotFinal:
			wrong = append(wrong, fmt.Sprintf("%s: module 1 reports a state that is not final (%s/%s)", short, r.Status, r.Reason))
		case res.Decision != d.Kind.String() || res.Unit != d.Unit || res.Rule != d.Rule:
			wrong = append(wrong, fmt.Sprintf("%s: the charge table makes %s/%s of module 1's answer, the result says %s/%s/%s", short, d.Kind, nonEmpty(d.Unit), res.Decision, nonEmpty(res.Unit), res.Rule))
		}
	}
	var out []string
	if len(none) > 0 {
		out = append(out, "items of module 1's report with no result: "+sample(none))
	}
	if len(wrong) > 0 {
		out = append(out, "results that differ from module 1's report: "+sample(wrong))
	}
	return out
}

func nonEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// reportEqualsLedger: the ledger charges an item exactly when the charge table charges module 1's answer for it, once, in the unit the table names; and holds no charge for an item the report does not name.
func reportEqualsLedger(report []m1.Row, charges []chargeInfo) []string {
	by := map[string][]chargeInfo{}
	for _, c := range charges {
		by[c.Ref] = append(by[c.Ref], c)
	}
	known := map[string]bool{}
	var wrong []string
	for _, r := range report {
		known[r.ItemID] = true
		d := charge.Decide(r.Outcome())
		got := by[r.ItemID]
		short := r.ItemID[:min(8, len(r.ItemID))]
		switch {
		case d.Kind == charge.Charge && len(got) != 1:
			wrong = append(wrong, fmt.Sprintf("%s: module 1's answer is a charge of %s and the ledger holds %s", short, d.Unit, plural(len(got), "charge row")))
		case d.Kind == charge.Charge && got[0].Unit != d.Unit:
			wrong = append(wrong, fmt.Sprintf("%s: the table says unit %s, the ledger charged %s", short, d.Unit, got[0].Unit))
		case d.Kind != charge.Charge && len(got) != 0:
			wrong = append(wrong, fmt.Sprintf("%s: module 1's answer is %s and the ledger charged it %d time(s)", short, d.Kind, len(got)))
		}
	}
	var stray []string
	for ref := range by {
		if !known[ref] {
			stray = append(stray, ref[:min(8, len(ref))])
		}
	}
	var out []string
	if len(wrong) > 0 {
		out = append(out, "ledger and module 1's report disagree: "+sample(wrong))
	}
	if len(stray) > 0 {
		out = append(out, "ledger charges for items module 1's report does not name: "+sample(stray))
	}
	return out
}

// ---- (f) the events -------------------------------------------------------------------------------------------------------------------------------------------------------------------

// The path of a job that was created, previewed, confirmed, started, completed and finished with nothing out of the ordinary: every hop once, in this order. A job that was killed and resumed
// takes the same path (a transaction a kill interrupts leaves no event), which is what makes "started twice" visible.
var normalPath = []string{"draft>previewed", "previewed>confirmed", "confirmed>running", "running>completing", "completing>succeeded"}

// eventsLegalAndOnceEach: every transition is a row of platform.job_transitions taken by an actor the row allows, the chain is unbroken from draft, no hop appears twice, the path is the normal one,
// and the last hop ends where the job is.
func eventsLegalAndOnceEach(evs []event, allowed map[string][]string, status string) []string {
	var out []string
	created := 0
	var path []string
	at := "draft"
	seen := map[string]int{}
	for _, e := range evs {
		switch e.Kind {
		case "created":
			created++
			continue
		case "transition":
		default:
			continue
		}
		hop := e.From + ">" + e.To
		actors, ok := allowed[hop]
		switch {
		case !ok:
			out = append(out, fmt.Sprintf("event %d: %s is no row of platform.job_transitions", e.ID, hop))
		case !contains(actors, e.Actor):
			out = append(out, fmt.Sprintf("event %d: %s taken by %s, the table allows %s", e.ID, hop, e.Actor, strings.Join(actors, "/")))
		}
		if e.From != at {
			out = append(out, fmt.Sprintf("event %d: %s does not follow the job's last status %s", e.ID, hop, at))
		}
		at = e.To
		seen[hop]++
		path = append(path, hop)
	}
	for hop, n := range seen {
		if n > 1 {
			out = append(out, fmt.Sprintf("the transition %s appears %d times (a job takes a hop once)", hop, n))
		}
	}
	if created != 1 {
		out = append(out, fmt.Sprintf("the job has %d `created` event(s), want 1", created))
	}
	if strings.Join(path, " ") != strings.Join(normalPath, " ") {
		out = append(out, fmt.Sprintf("the job's path is [%s], want [%s]", strings.Join(path, " "), strings.Join(normalPath, " ")))
	}
	if at != status {
		out = append(out, fmt.Sprintf("the events end at %s and the job is %s", at, status))
	}
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// ---- verify's own control -------------------------------------------------------------------------------------------------------------------------------------------------------------

// controlDetectsCorruption builds a small consistent job (two module-answered rows, one malformed, one repeat), proves that every check above passes on it, and then corrupts it one way at a time and
// requires the check that guards that property to find a problem. It returns what failed to behave: nothing when every check can fail and none fails on a good job.
func controlDetectsCorruption() []string {
	var failed []string
	id := func(n int) string { return fmt.Sprintf("%08x-0000-4000-8000-000000000000", n) }
	rows := []rowInfo{
		{ID: id(1), Ordinal: 1, State: "pending", Email: "a@d1.example.test"},
		{ID: id(2), Ordinal: 2, State: "malformed", Email: "no-at-sign"},
		{ID: id(3), Ordinal: 3, State: "pending", Email: "b@free.example.test"},
		{ID: id(4), Ordinal: 4, State: "duplicate", Email: "a@d1.example.test"},
	}
	results := []resultInfo{
		{Ref: id(1), Email: "a@d1.example.test", Origin: "module", State: "invalid", Reason: "no_mx", Decision: "charge", Unit: "m1_own", Rule: "M1-05", Ordinal: 1},
		{Ref: id(2), Email: "no-at-sign", Origin: "platform", State: "malformed", Decision: "free", Rule: "P-08", Ordinal: 2},
		{Ref: id(3), Email: "b@free.example.test", Origin: "module", State: "risky", Reason: "plausible", Decision: "free", Rule: "M1-17", Ordinal: 3},
		{Ref: id(4), Email: "a@d1.example.test", Origin: "platform", State: "duplicate", Decision: "free", Rule: "P-07", Ordinal: 4},
	}
	charges := []chargeInfo{{Ref: id(1), Unit: "m1_own", Units: 1}}
	report := []m1.Row{
		{Ordinal: 0, ItemID: id(1), Status: "invalid", Reason: "no_mx"},
		{Ordinal: 1, ItemID: id(3), Status: "risky", Reason: "plausible", SkipReason: "smtp_unwired"},
	}
	pending := map[string]rowInfo{id(1): rows[0], id(3): rows[2]}
	byRef := map[string]resultInfo{}
	for _, r := range results {
		byRef[r.Ref] = r
	}
	facts := reserveFacts{Reserves: 1, Releases: 1, ReleasedCredits: 1, Reserved: 2, Charged: 1, Released: 1, Granted: 100, Available: 99, ChargedUnitCostSum: 1}
	hop := func(id int64, from, to, actor string) event {
		return event{ID: id, Kind: "transition", From: from, To: to, Actor: actor}
	}
	allowed := map[string][]string{"draft>previewed": {"user", "admin"}, "previewed>confirmed": {"user", "admin"}, "confirmed>running": {"engine"}, "running>completing": {"engine"},
		"completing>succeeded": {"engine"}, "running>failed": {"admin", "engine"}}
	evs := []event{{ID: 1, Kind: "created", Actor: "user"}, hop(2, "draft", "previewed", "user"), hop(3, "previewed", "confirmed", "user"), hop(4, "confirmed", "running", "engine"),
		hop(5, "running", "completing", "engine"), hop(6, "completing", "succeeded", "engine")}

	// the good job passes every check
	good := map[string][]string{
		"rowsEqualResults": rowsEqualResults(rows, results), "everyPendingRowHasOneResult": everyPendingRowHasOneResult(rows, results), "resultsNameTheirRows": resultsNameTheirRows(rows, results),
		"atMostOneChargePerItem": atMostOneChargePerItem(charges), "chargesEqualResultsWithAUnit": chargesEqualResultsWithAUnit(charges, results),
		"reserveClosed": reserveClosed(facts), "oneReleasePerReserve": oneReleasePerReserve(facts), "reportCovers": reportCovers(report, pending),
		"reportEqualsResults": reportEqualsResults(report, byRef), "reportEqualsLedger": reportEqualsLedger(report, charges), "eventsLegalAndOnceEach": eventsLegalAndOnceEach(evs, allowed, "succeeded"),
	}
	for name, ps := range good {
		if len(ps) != 0 {
			failed = append(failed, fmt.Sprintf("the check %s found a problem in the GOOD fixture: %s", name, strings.Join(ps, "; ")))
		}
	}
	// each corruption is found by the check that guards it
	expect := func(what string, ps []string) {
		if len(ps) == 0 {
			failed = append(failed, "the corruption "+what+" was NOT found")
		}
	}
	expect("a result is missing (rows != results)", rowsEqualResults(rows, results[:3]))
	expect("a pending row has no result", everyPendingRowHasOneResult(rows, append(append([]resultInfo{}, results[:2]...), results[3])))
	expect("a pending row has two results", everyPendingRowHasOneResult(rows, append(append([]resultInfo{}, results...), results[0])))
	wrongName := append([]resultInfo{}, results...)
	wrongName[0].Email = "someone-else@d1.example.test"
	expect("a result names another address", resultsNameTheirRows(rows, wrongName))
	expect("an item is charged twice", atMostOneChargePerItem(append(append([]chargeInfo{}, charges...), charges[0])))
	expect("a charge has no result with a unit", chargesEqualResultsWithAUnit(append(append([]chargeInfo{}, charges...), chargeInfo{Ref: id(3), Unit: "m1_own", Units: 1}), results))
	expect("a result with a unit has no charge", chargesEqualResultsWithAUnit(nil, results))
	stuck := facts
	stuck.Released = 0
	expect("the reserve is not closed", reserveClosed(stuck))
	twice := facts
	twice.Releases = 2
	expect("the remainder is released twice", oneReleasePerReserve(twice))
	expect("the report loses a pending row", reportCovers(report[:1], pending))
	expect("the report names an item that is no row", reportCovers(append(append([]m1.Row{}, report...), m1.Row{Ordinal: 2, ItemID: id(9), Status: "valid", Reason: "accepted"}), pending))
	flipped := append([]m1.Row{}, report...)
	flipped[1] = m1.Row{Ordinal: 1, ItemID: id(3), Status: "invalid", Reason: "no_mx"} // module 1 now says charge, the result says free
	expect("module 1's report differs from the results", reportEqualsResults(flipped, byRef))
	expect("module 1's report implies a charge the ledger lacks", reportEqualsLedger(flipped, charges))
	expect("the ledger charges an item the report does not name", reportEqualsLedger(report[:1], append(append([]chargeInfo{}, charges...), chargeInfo{Ref: id(3), Unit: "m1_own", Units: 1})))
	again := append(append([]event{}, evs[:4]...), hop(7, "confirmed", "running", "engine"))
	expect("the job is started twice", eventsLegalAndOnceEach(append(append([]event{}, evs...), again[4]), allowed, "succeeded"))
	illegal := append(append([]event{}, evs[:5]...), hop(6, "completing", "previewed", "engine"))
	expect("a transition the table does not allow", eventsLegalAndOnceEach(illegal, allowed, "previewed"))
	byUser := append(append([]event{}, evs[:3]...), hop(4, "confirmed", "running", "user"), evs[4], evs[5])
	expect("a transition taken by an actor the table does not allow", eventsLegalAndOnceEach(byUser, allowed, "succeeded"))
	sort.Strings(failed)
	return failed
}
