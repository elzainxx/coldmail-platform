//go:build harness && m1node && m2worker

package harness

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"

	m1ad "github.com/coldmail-platform/platform/internal/adapters/m1"
	"github.com/coldmail-platform/platform/internal/jobs"
)

// The comparisons of the chain test (m1_node_chain_real_test.go) and the test that proves they can fail. Behind the build tags harness, m1node AND m2worker: the chain from module 1's REAL
// verdicts into a REAL module 2 send needs the node stack (m1node) and the module 2 worker with its sink (m2worker), so it is compiled only into the binary stage M1N-4c of
// tests/harness/m1node/m1node.sh builds when module 2 is part of the run; no other binary contains it (the m1node stage's ordinary binary has no m2worker tag, the full harness binary no m1node
// tag). Every name starts with rch so that it cannot clash with another file of this package.
//
// Pure functions: they take what the live test READ (the platform's rows, results and ledger, module 1's own report, module 2's report, the sink's counters) and return every disagreement as a
// sentence, never failing a test themselves, so that the SAME function is run on a corrupted copy of what it just accepted and shown to report the corruption (TestM1Node_ChainChecksBite does it
// on a synthetic world that needs no stack, the live test does it on the real data). A comparison that cannot fail proves nothing. No sentence carries an address: a row is named by its recipe.

// rchSpec is one row of a source list: a recipe of the shared table (m1nRecipe.id of m1_node_test.go: which address gets which answer from module 1's real node and the fixtures) and the verdict
// CLASS that the hand-written rule says module 1 must give it. The class is written here by hand so that a wrong edit of the table, or a module that changes its mind, is red.
type rchSpec struct{ id, class string }

// rchSourceSpecs is the source list of the chain: two addresses of each of the four verdict classes, interleaved so that no class is a block of the list. Five of the eight reach the node (two RCPTs
// each at the fixture: the target and the probe); the other three are answered by module 1's own layers (a list, the lack of an MX, a SERVFAIL).
var rchSourceSpecs = []rchSpec{
	{"valid", "valid"},
	{"mailbox_not_found", "invalid"},
	{"greylisted", "unknown"},
	{"catch_all", "risky"},
	{"mailbox_full", "valid"},
	{"no_mx", "invalid"},
	{"dns_servfail_remote", "unknown"},
	{"disposable", "risky"},
}

// rchSmallSpecs is a source with NO unknown row: the chain of such a source needs no choice.
var rchSmallSpecs = []rchSpec{
	{"mailbox_not_found", "invalid"},
	{"valid", "valid"},
	{"catch_all", "risky"},
}

// rchAddr is one address of a source list and everything the test knows about it BY HAND, independently of what module 1 reports.
type rchAddr struct {
	addr    string
	id      string     // the recipe
	class   string     // valid, risky, invalid or unknown: what module 1 must say, by the hand-written rule
	reaches bool       // the node is called for it
	want    m1eVerdict // the recipe table's own words (status, reason, failure origin, skip token)
}

// rchPlan makes the addresses of one source list: <prefix>-<recipe>@<the recipe's domain>. The prefix is unique to this run, so no address was ever verified (the cache is keyed by address) and none
// was ever sent to (the sink's counters are read by this prefix).
func rchPlan(t *testing.T, e *m1nEnv, prefix string, specs []rchSpec) []rchAddr {
	t.Helper()
	byID := map[string]m1nRecipe{}
	for _, r := range e.recipes() {
		byID[r.id] = r
	}
	out := make([]rchAddr, len(specs))
	for i, sp := range specs {
		r, ok := byID[sp.id]
		if !ok {
			t.Fatalf("the recipe %q is not in the shared recipe table (m1_node_test.go): this test's source list names a recipe that does not exist", sp.id)
		}
		if r.want.status != sp.class {
			t.Fatalf("the recipe table says %q gives a %s verdict, this test's hand-written class for it is %s: one of the two is wrong", sp.id, r.want.status, sp.class)
		}
		out[i] = rchAddr{addr: fmt.Sprintf("%s-%s@%s", prefix, strings.ReplaceAll(sp.id, "_", "-"), r.domain), id: sp.id, class: sp.class, reaches: r.reaches, want: m1eVerdictOfWant(r.want)}
	}
	return out
}

func rchAddrs(plan []rchAddr) []string {
	out := make([]string, len(plan))
	for i, a := range plan {
		out[i] = a.addr
	}
	return out
}

// rchReaching are the addresses of the plan the node is called for.
func rchReaching(plan []rchAddr) []string {
	var out []string
	for _, a := range plan {
		if a.reaches {
			out = append(out, a.addr)
		}
	}
	return out
}

// rchSelectedSet is the set of addresses of the plan the owner's rule selects (jlSelects, written by hand), by the plan's own classes and not by what module 1 or the store said.
func rchSelectedSet(plan []rchAddr, includeUnknown bool) map[string]bool {
	out := map[string]bool{}
	for _, a := range plan {
		if jlSelects(a.class, includeUnknown) {
			out[a.addr] = true
		}
	}
	return out
}

func rchCount(plan []rchAddr, class string) int {
	n := 0
	for _, a := range plan {
		if a.class == class {
			n++
		}
	}
	return n
}

// ---- assertion 1: the verdicts are real, and three places say the same ---------------------------------------------------------------------------------------------------------------

// rchVerdictProblems: for every address of the plan the job has ONE row, ONE result of origin module whose verdict is the hand-written class and whose reason is the recipe table's, and module 1's OWN
// report (read with the user's key) says exactly the status, reason, failure origin and skip token of the table for it, not from the cache; and the job holds nothing else.
func rchVerdictProblems(plan []rchAddr, rows []jlRow, results []jlResult, report []m1ad.Row) []string {
	var p []string
	say := func(f string, a ...any) { p = append(p, fmt.Sprintf(f, a...)) }
	rowOf := map[string]jlRow{}
	for _, r := range rows {
		rowOf[r.Norm] = r
	}
	resOf := jlByRef(results)
	repOf := map[string][]m1ad.Row{}
	for _, m := range report {
		repOf[m.ItemID] = append(repOf[m.ItemID], m)
	}
	for _, a := range plan {
		row, ok := rowOf[a.addr]
		if !ok {
			say("the recipe %s (a %s verdict): no row of the job carries its address", a.id, a.class)
			continue
		}
		if rs := resOf[row.ID]; len(rs) != 1 {
			say("the recipe %s: the job holds %d result(s) for its row, want exactly 1", a.id, len(rs))
		} else {
			r := rs[0]
			if r.Origin != "module" {
				say("the recipe %s: its result is of origin %q, want module (module 1 answered it)", a.id, r.Origin)
			}
			if r.State != a.class {
				say("the recipe %s: the result says the verdict %s, the hand-written class is %s", a.id, r.State, a.class)
			}
			if r.Reason != a.want.reason {
				say("the recipe %s: the result says the reason %q, the recipe table says %q", a.id, r.Reason, a.want.reason)
			}
		}
		ms := repOf[row.ID]
		if len(ms) != 1 {
			say("the recipe %s: module 1's own report holds %d row(s) for it, want exactly 1", a.id, len(ms))
			continue
		}
		if got := m1eVerdictOfRow(ms[0]); got != a.want {
			say("the recipe %s: module 1's own report says %s, the recipe table says %s", a.id, got, a.want)
		}
	}
	if len(rows) != len(plan) || len(results) != len(plan) || len(report) != len(plan) {
		say("the job holds %d row(s), %d result(s) and module 1's report %d row(s) for a list of %d address(es)", len(rows), len(results), len(report), len(plan))
	}
	sort.Strings(p)
	return p
}

// ---- the preview and the rows of a chain -------------------------------------------------------------------------------------------------------------------------------------

// rchPreviewProblems: the preview of a chained module 2 send says what the source's verdicts and the choice make of the list, by the owner's rule: every verdict counted, the selected ones pending, the rest
// excluded, the unknown results counted, a choice required exactly when there is one, and the credits the reserve will hold at the dearest reachable unit (module 2 is metered). choice "" is "not made yet":
// the unknown rows are then shown excluded, the safe reading.
func rchPreviewProblems(src []jlSrc, pv jobs.Preview, choice string) []string {
	var p []string
	say := func(f string, a ...any) { p = append(p, fmt.Sprintf(f, a...)) }
	want := map[string]int{"valid": 0, "invalid": 0, "risky": 0, "unknown": 0}
	selected := 0
	for _, x := range src {
		want[x.State]++
		if jlSelects(x.State, choice == jobs.UnknownInclude) {
			selected++
		}
	}
	for v, n := range want {
		if pv.Verdicts[v] != n {
			say("the preview counts %d result(s) of verdict %s, the source job holds %d", pv.Verdicts[v], v, n)
		}
	}
	if pv.Rows != len(src) || pv.Selected != selected || pv.Pending != selected || pv.Units != selected || pv.Excluded != len(src)-selected || pv.Malformed != 0 || pv.Duplicates != 0 || pv.Suppressed != 0 {
		say("the preview says rows %d selected %d pending %d units %d excluded %d malformed %d repeats %d suppressed %d, want rows %d, selected, pending and units %d, excluded %d, no malformed, repeated or suppressed row",
			pv.Rows, pv.Selected, pv.Pending, pv.Units, pv.Excluded, pv.Malformed, pv.Duplicates, pv.Suppressed, len(src), selected, len(src)-selected)
	}
	if pv.UnknownCount != want["unknown"] || pv.UnknownChoiceRequired != (want["unknown"] > 0) || pv.UnknownChoice != choice {
		say("the preview says %d unknown result(s), a choice required %v, the recorded choice %q; want %d, %v and %q", pv.UnknownCount, pv.UnknownChoiceRequired, pv.UnknownChoice, want["unknown"], want["unknown"] > 0, choice)
	}
	if !pv.Metered || pv.UnitCost < 1 || pv.Credits != int64(selected)*int64(pv.UnitCost) || pv.Shortfall != 0 {
		say("the preview says metered %v, %d credit(s) at %d per unit with a shortfall of %d, want module 2's reserve: %d unit(s) x the rate, metered, no shortfall", pv.Metered, pv.Credits, pv.UnitCost, pv.Shortfall, selected)
	}
	sort.Strings(p)
	return p
}

// rchRowsAreSource: the chained job's rows are exactly the source job's module results, one for one, in the source's order: the same count, the same address at every position and the source item
// each row names. (Which of them are pending is jlChainRowsProblems's: the owner's rule over the verdicts.)
func rchRowsAreSource(src []jlSrc, rows []jlRow) []string {
	var p []string
	say := func(f string, a ...any) { p = append(p, fmt.Sprintf(f, a...)) }
	if len(rows) != len(src) {
		say("the chained job has %d row(s) for the %d module result(s) of its source", len(rows), len(src))
	}
	for i := 0; i < len(rows) && i < len(src); i++ {
		if rows[i].Norm != src[i].Email {
			say("row %d of the chained job is not the address of source result %d", i+1, i+1)
		}
		if rows[i].Source != src[i].Ref {
			say("row %d of the chained job names another source item than result %d of the source job", i+1, i+1)
		}
	}
	sort.Strings(p)
	return p
}

// rchDigest is a digest of everything a job's results say, whatever the order they were read in: two equal values mean the results did not change.
func rchDigest(res []jlResult) string {
	lines := make([]string, len(res))
	for i, r := range res {
		lines[i] = strings.Join([]string{r.Ref, strconv.Itoa(r.Ordinal), r.Email, r.Origin, r.State, r.Reason, r.Decision, r.Unit, r.Rule}, "|")
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strconv.Itoa(len(lines)) + "\n" + strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}

// ---- the sink ------------------------------------------------------------------------------------------------------------------------------------------------------------------------

// rchSinkView is what the controllable sink holds for the addresses of this run (the ones that start with the run's prefix): the messages and the RCPT TO answers, per exact address.
type rchSinkView struct {
	Msgs map[string]int
	Rcpt map[string]m2wRcpt
}

func (v rchSinkView) clone() rchSinkView {
	w := rchSinkView{Msgs: map[string]int{}, Rcpt: map[string]m2wRcpt{}}
	for k, x := range v.Msgs {
		w.Msgs[k] = x
	}
	for k, x := range v.Rcpt {
		w.Rcpt[k] = x
	}
	return w
}

// rchDelta is what the sink gained between two readings, per address: the messages and the RCPT TO answers (jlM2View's own shape).
func rchDelta(before, after rchSinkView) (map[string]int, map[string]m2wRcpt) {
	msgs, rcpt := map[string]int{}, map[string]m2wRcpt{}
	for a, n := range after.Msgs {
		msgs[a] = n - before.Msgs[a]
	}
	for a, c := range after.Rcpt {
		b := before.Rcpt[a]
		rcpt[a] = m2wRcpt{Accepted: c.Accepted - b.Accepted, Rejected: c.Rejected - b.Rejected, Deferred: c.Deferred - b.Deferred}
	}
	return msgs, rcpt
}

// rchSinkProblems: for EVERY address of the plan, what the sink gained between the two readings is exactly what the owner's rule says: a selected address one message and one accepted RCPT TO, any other
// address (invalid always, unknown unless included) no message AND no RCPT TO at all (module 2 was never asked to send to it); and nothing at the sink carries the run's prefix without being an address of the
// plan. The selection is the plan's hand-written classes through jlSelects, never what the job or module 1 said.
func rchSinkProblems(plan []rchAddr, includeUnknown bool, before, after rchSinkView) []string {
	var p []string
	say := func(f string, a ...any) { p = append(p, fmt.Sprintf(f, a...)) }
	known := map[string]bool{}
	for _, a := range plan {
		known[a.addr] = true
		sel := jlSelects(a.class, includeUnknown)
		dm := after.Msgs[a.addr] - before.Msgs[a.addr]
		b, c := before.Rcpt[a.addr], after.Rcpt[a.addr]
		dr := m2wRcpt{Accepted: c.Accepted - b.Accepted, Rejected: c.Rejected - b.Rejected, Deferred: c.Deferred - b.Deferred}
		wantM, wantR := 0, m2wRcpt{}
		if sel {
			wantM, wantR = 1, m2wRcpt{Accepted: 1}
		}
		if dm != wantM || dr != wantR {
			say("the address of the recipe %s (a %s verdict, selected=%v) gained %d message(s) at the sink and was answered at RCPT TO accepted %d rejected %d deferred %d, want %d message(s) and accepted %d rejected 0 deferred 0",
				a.id, a.class, sel, dm, dr.Accepted, dr.Rejected, dr.Deferred, wantM, wantR.Accepted)
		}
	}
	for a, n := range after.Msgs {
		if !known[a] && n != before.Msgs[a] {
			say("the sink gained %d message(s) to an address of this run that no row of the source owns", n-before.Msgs[a])
		}
	}
	for a, c := range after.Rcpt {
		if !known[a] && c != before.Rcpt[a] {
			say("the sink answered RCPT TO for an address of this run that no row of the source owns")
		}
	}
	sort.Strings(p)
	return p
}

// rchRound is one chained send of the test: the source list it chained to and whether the user included the unknown verdicts (false too for a source with none).
type rchRound struct {
	plan    []rchAddr
	include bool
}

// rchExpectedSends is, by the hand-written rule, how many times each address of the rounds was sent to: once per round that selected it, 0 for an address no round selected.
func rchExpectedSends(rounds []rchRound) map[string]int {
	want := map[string]int{}
	for _, rd := range rounds {
		for _, a := range rd.plan {
			if _, ok := want[a.addr]; !ok {
				want[a.addr] = 0
			}
			if jlSelects(a.class, rd.include) {
				want[a.addr]++
			}
		}
	}
	return want
}

// rchCumulativeProblems: at the end of the test, from a sink that held nothing of this run at the start, the sink holds for EVERY address exactly as many messages (and accepted RCPT TO answers) as the
// rounds that selected it, and nothing at all (no message, no RCPT TO answer) for an address no round selected, the invalid ones above all; and nothing carries the run's prefix that is not an address of a round.
func rchCumulativeProblems(rounds []rchRound, final rchSinkView) []string {
	var p []string
	say := func(f string, a ...any) { p = append(p, fmt.Sprintf(f, a...)) }
	want := rchExpectedSends(rounds)
	for _, rd := range rounds {
		for _, a := range rd.plan {
			n, c := final.Msgs[a.addr], final.Rcpt[a.addr]
			if n != want[a.addr] || c != (m2wRcpt{Accepted: want[a.addr]}) {
				say("the address of the recipe %s (a %s verdict) holds %d message(s) at the sink and was answered at RCPT TO accepted %d rejected %d deferred %d, want %d message(s) in all (one per chained send that selected it) and accepted %d",
					a.id, a.class, n, c.Accepted, c.Rejected, c.Deferred, want[a.addr], want[a.addr])
			}
		}
	}
	for a, n := range final.Msgs {
		if _, ok := want[a]; !ok && n != 0 {
			say("the sink holds %d message(s) to an address of this run that no chained send owns", n)
		}
	}
	for a, c := range final.Rcpt {
		if _, ok := want[a]; !ok && c != (m2wRcpt{}) {
			say("the sink answered RCPT TO for an address of this run that no chained send owns")
		}
	}
	sort.Strings(p)
	return p
}

// rchModuleCountProblems: module 2's OWN tables, read as the superuser, hold for every address of the rounds exactly as many module rows (public.job_items) and module jobs (send_managed.jobs) as the
// rounds that selected it: an address the owner's rule never selects (the invalid ones) never reached module 2 at all, not even as a row it refused.
func rchModuleCountProblems(rounds []rchRound, items, mjobs map[string]int64) []string {
	var p []string
	say := func(f string, a ...any) { p = append(p, fmt.Sprintf(f, a...)) }
	want := rchExpectedSends(rounds)
	byAddr := map[string]rchAddr{}
	for _, rd := range rounds {
		for _, a := range rd.plan {
			byAddr[a.addr] = a
		}
	}
	for addr, n := range want {
		a := byAddr[addr]
		if items[addr] != int64(n) || mjobs[addr] != int64(n) {
			say("the address of the recipe %s (a %s verdict) has %d row(s) in public.job_items and %d job(s) in module 2's own table, want %d of each (one per chained send that selected it)", a.id, a.class, items[addr], mjobs[addr], n)
		}
	}
	for addr, n := range items {
		if _, ok := want[addr]; !ok && n != 0 {
			say("public.job_items holds %d row(s) for an address of this run that no chained send owns", n)
		}
	}
	for addr, n := range mjobs {
		if _, ok := want[addr]; !ok && n != 0 {
			say("module 2 holds %d job(s) for an address of this run that no chained send owns", n)
		}
	}
	sort.Strings(p)
	return p
}

// ---- assertion 4: a refused confirm writes nothing -----------------------------------------------------------------------------------------------------------------------------

// rchState is everything a confirm writes, read about one chained job and its user.
type rchState struct {
	Status      string
	ReserveHeld bool
	LedgerRows  int64 // ledger rows of the job
	Results     int   // job_results rows
	JobItems    int64 // public.job_items rows of the campaign
	ModuleJobs  int64 // module 2's own jobs of the campaign
	ActiveM2    int64 // the user's module 2 jobs in a slot state
	ConfirmKey  string
	Confirmed   int64 // moves of the job to confirmed
	Available   int64 // the user's module 2 balance
	Reserved    int64
}

// rchNothingProblems: the state of a job whose confirm was refused: still previewed, no reserve, no ledger row, no result, no module row, no module job, the slot free, no confirm key, no move to
// confirmed, and the user's module 2 balance exactly what it was (avail0, rsv0).
func rchNothingProblems(st rchState, avail0, rsv0 int64) []string {
	var p []string
	say := func(f string, a ...any) { p = append(p, fmt.Sprintf(f, a...)) }
	if st.Status != jobs.StatusPreviewed {
		say("the job is %s, want previewed", st.Status)
	}
	if st.ReserveHeld || st.LedgerRows != 0 {
		say("the job holds a reserve (%v) and %d ledger row(s), want none", st.ReserveHeld, st.LedgerRows)
	}
	if st.Results != 0 {
		say("the job holds %d result(s): a refused confirm writes none", st.Results)
	}
	if st.JobItems != 0 || st.ModuleJobs != 0 {
		say("public.job_items holds %d row(s) of the campaign and module 2 holds %d job(s) of it, want none", st.JobItems, st.ModuleJobs)
	}
	if st.ActiveM2 != 0 {
		say("the user holds %d active module 2 job(s): the slot must be free", st.ActiveM2)
	}
	if st.ConfirmKey != "" || st.Confirmed != 0 {
		say("the job carries a confirm key (%v) or moved to confirmed %d time(s)", st.ConfirmKey != "", st.Confirmed)
	}
	if st.Available != avail0 || st.Reserved != rsv0 {
		say("the user's module 2 balance is available %d reserved %d, it was %d and %d", st.Available, st.Reserved, avail0, rsv0)
	}
	sort.Strings(p)
	return p
}

// ---- the proof that every comparison above can fail -------------------------------------------------------------------------------------------------------------------------------

// TestM1Node_ChainChecksBite needs no stack. It builds a small CONSISTENT world (a source list, its rows, results and module 1's report, a chain's rows, a sink before and after, a refused confirm) and
// requires every comparison of the chain test to accept it and then to report, naming the reason, each way it can be wrong. It also pins the lists the live test sends: every recipe they name is in the
// shared table with the class written here, all four classes are present, the small source has no unknown row.
func TestM1Node_ChainChecksBite(t *testing.T) {
	e := &m1nEnv{nodeIP: "198.51.100.3", smtpIP: "198.51.100.2"}
	plan := rchPlan(t, e, "d4bite-a", rchSourceSpecs)
	small := rchPlan(t, e, "d4bite-b", rchSmallSpecs)

	// the lists
	for _, c := range []string{"valid", "risky", "invalid", "unknown"} {
		if rchCount(plan, c) != 2 {
			t.Fatalf("the source list holds %d address(es) of the class %s, want 2: the chain must be shown to move or to hold each class", rchCount(plan, c), c)
		}
	}
	if rchCount(small, "unknown") != 0 || rchCount(small, "valid") < 1 || rchCount(small, "risky") < 1 || rchCount(small, "invalid") < 1 {
		t.Fatalf("the small source must hold a valid, a risky and an invalid address and no unknown one")
	}
	if n := len(rchReaching(plan)); n != 5 {
		t.Fatalf("%d address(es) of the source list reach the node, want 5 (valid, mailbox_full, mailbox_not_found, catch_all and greylisted open an SMTP session at the fixture; the other three never leave module 1): the recipe table changed", n)
	}
	seen := map[string]bool{}
	for _, a := range append(append([]rchAddr{}, plan...), small...) {
		if seen[a.addr] {
			t.Fatalf("two addresses of the lists are the same: a repeat would be one check")
		}
		seen[a.addr] = true
	}
	if len(rchSelectedSet(plan, false)) != 4 || len(rchSelectedSet(plan, true)) != 6 {
		t.Fatalf("the owner's rule selects %d (without unknown) and %d (with unknown) of the source list, want 4 and 6", len(rchSelectedSet(plan, false)), len(rchSelectedSet(plan, true)))
	}

	// the synthetic world
	n := len(plan)
	rows := make([]jlRow, n)
	results := make([]jlResult, n)
	report := make([]m1ad.Row, n)
	src := make([]jlSrc, n)
	for i, a := range plan {
		id := jlID(i + 1)
		rows[i] = jlRow{ID: id, Ordinal: i + 1, Norm: a.addr, State: "pending", Submitted: true}
		results[i] = jlResult{Ref: id, Ordinal: i + 1, Email: a.addr, Origin: "module", State: a.class, Reason: a.want.reason, Decision: "charge", Unit: "m1_own", Rule: "M1-03"}
		report[i] = m1ad.Row{Ordinal: i, ItemID: id, Status: a.want.status, Reason: a.want.reason, FailureOrigin: a.want.origin, SkipReason: a.want.skip}
		src[i] = jlSrc{Ref: id, Email: a.addr, State: a.class}
	}
	cloneRows := func() []jlRow { return append([]jlRow(nil), rows...) }
	cloneResults := func() []jlResult { return append([]jlResult(nil), results...) }
	cloneReport := func() []m1ad.Row { return append([]m1ad.Row(nil), report...) }

	// assertion 1: the verdicts
	if p := rchVerdictProblems(plan, rows, results, report); len(p) != 0 {
		t.Fatalf("control: the consistent world must pass the verdict check, got %v", p)
	}
	{
		w := cloneResults()
		w[0].State = "invalid"
		jlWantProblem(t, "a result's verdict changed", "the hand-written class is valid", rchVerdictProblems(plan, rows, w, report))
		w = cloneResults()
		w[2].Reason = "dns_error"
		jlWantProblem(t, "a result's reason changed", "the recipe table says", rchVerdictProblems(plan, rows, w, report))
		w = cloneResults()
		w[1].Origin = "platform"
		jlWantProblem(t, "a platform result claimed to be the module's answer", "want module", rchVerdictProblems(plan, rows, w, report))
		jlWantProblem(t, "a result dropped", "holds 0 result(s)", rchVerdictProblems(plan, rows, cloneResults()[1:], report))
		r := cloneReport()
		r[3].Reason = "plausible"
		jlWantProblem(t, "a row of module 1's own report with another reason", "module 1's own report says", rchVerdictProblems(plan, rows, results, r))
		r = cloneReport()
		r[4].FromCache = true
		jlWantProblem(t, "a row of module 1's own report answered from the cache", "module 1's own report says", rchVerdictProblems(plan, rows, results, r))
		jlWantProblem(t, "a row of module 1's own report dropped", "module 1's own report holds 0 row(s)", rchVerdictProblems(plan, rows, results, cloneReport()[1:]))
		wrong := append([]rchAddr(nil), plan...)
		wrong[0].class = "invalid" // the hand-written class contradicts what the job holds
		jlWantProblem(t, "the hand-written class of one address is wrong", "the hand-written class is invalid", rchVerdictProblems(wrong, rows, results, report))
		jlWantProblem(t, "a row the job never had", "no row of the job carries its address", rchVerdictProblems(plan, cloneRows()[1:], results, report))
	}

	// the preview of a chain
	pv := jobs.Preview{
		Rows: n, Pending: 4, Excluded: 4, Units: 4, Selected: 4, Verdicts: map[string]int{"valid": 2, "risky": 2, "invalid": 2, "unknown": 2},
		UnknownChoice: jobs.UnknownExclude, UnknownCount: 2, UnknownChoiceRequired: true, Metered: true, RateUnit: "m2_accepted", UnitCost: 1, Credits: 4, Available: 100,
	}
	if p := rchPreviewProblems(src, pv, jobs.UnknownExclude); len(p) != 0 {
		t.Fatalf("control: the consistent preview must pass, got %v", p)
	}
	{
		w := pv
		w.Verdicts = map[string]int{"valid": 2, "risky": 2, "invalid": 3, "unknown": 1}
		jlWantProblem(t, "a verdict miscounted", "of verdict unknown", rchPreviewProblems(src, w, jobs.UnknownExclude))
		w = pv
		w.Selected, w.Pending, w.Units, w.Credits = 6, 6, 6, 6
		jlWantProblem(t, "the unknown rows selected under exclude", "the preview says rows", rchPreviewProblems(src, w, jobs.UnknownExclude))
		w = pv
		w.UnknownChoiceRequired = false
		jlWantProblem(t, "no choice required although two results are unknown", "a choice required false", rchPreviewProblems(src, w, jobs.UnknownExclude))
		w = pv
		w.Credits = 3
		jlWantProblem(t, "the credits are not the units times the rate", "want module 2's reserve", rchPreviewProblems(src, w, jobs.UnknownExclude))
		w = pv
		w.Metered = false
		jlWantProblem(t, "module 2 not metered", "want module 2's reserve", rchPreviewProblems(src, w, jobs.UnknownExclude))
		jlWantProblem(t, "the preview judged under the other choice", "the preview says", rchPreviewProblems(src, pv, jobs.UnknownInclude))
	}

	// the rows of a chain: the source's rows, one for one
	chain := make([]jlRow, n)
	for i, a := range plan {
		st, why := "excluded", a.class
		if jlSelects(a.class, false) {
			st, why = "pending", ""
		}
		chain[i] = jlRow{ID: jlID(100 + i), Ordinal: i + 1, Norm: a.addr, State: st, Reason: why, Source: jlID(i + 1)}
	}
	cloneChain := func() []jlRow { return append([]jlRow(nil), chain...) }
	if p := rchRowsAreSource(src, chain); len(p) != 0 {
		t.Fatalf("control: the consistent chain must be the source's rows, got %v", p)
	}
	if p := jlChainRowsProblems(src, chain, false); len(p) != 0 {
		t.Fatalf("control: the consistent chain must follow the owner's rule under exclude, got %v", p)
	}
	jlWantProblem(t, "the exclude chain judged by include", "became a chain row in state", jlChainRowsProblems(src, chain, true))
	{
		jlWantProblem(t, "a row dropped from the chain", "row(s) for the", rchRowsAreSource(src, cloneChain()[1:]))
		jlWantProblem(t, "a row added to the chain", "row(s) for the", rchRowsAreSource(src, append(cloneChain(), jlRow{ID: jlID(900), Ordinal: n + 1, Norm: "x@y", State: "pending"})))
		w := cloneChain()
		w[2].Norm = w[3].Norm
		jlWantProblem(t, "a row with another address", "is not the address of source result 3", rchRowsAreSource(src, w))
		w = cloneChain()
		w[1].Source = jlID(77)
		jlWantProblem(t, "a row naming another source item", "names another source item", rchRowsAreSource(src, w))
		w = cloneChain()
		w[0], w[1] = w[1], w[0]
		jlWantProblem(t, "two rows in another order", "is not the address of source result", rchRowsAreSource(src, w))
	}

	// assertion 5: the digest of the source's results
	d := rchDigest(results)
	if rchDigest(cloneResults()) != d {
		t.Fatalf("control: the digest of a copy must be the digest of the original")
	}
	rev := cloneResults()
	rev[0], rev[1] = rev[1], rev[0]
	if rchDigest(rev) != d {
		t.Fatalf("control: the digest must not depend on the order the results were read in")
	}
	for name, mut := range map[string]func(r []jlResult) []jlResult{
		"a verdict changed":   func(r []jlResult) []jlResult { r[0].State = "invalid"; return r },
		"a decision changed":  func(r []jlResult) []jlResult { r[0].Decision = "free"; return r },
		"a result dropped":    func(r []jlResult) []jlResult { return r[1:] },
		"a result duplicated": func(r []jlResult) []jlResult { return append(r, r[0]) },
		"a rule changed":      func(r []jlResult) []jlResult { r[2].Rule = "P-EXC"; return r },
	} {
		if rchDigest(mut(cloneResults())) == d {
			t.Errorf("the digest did not change when %s: a source whose results changed would be called untouched", name)
		}
	}

	// assertion 2 and 3: the sink
	zero := rchSinkView{Msgs: map[string]int{}, Rcpt: map[string]m2wRcpt{}}
	afterExclude := zero.clone()
	for a := range rchSelectedSet(plan, false) {
		afterExclude.Msgs[a] = 1
		afterExclude.Rcpt[a] = m2wRcpt{Accepted: 1}
	}
	afterInclude := zero.clone()
	for a := range rchSelectedSet(plan, true) {
		afterInclude.Msgs[a] = 1
		afterInclude.Rcpt[a] = m2wRcpt{Accepted: 1}
	}
	if p := rchSinkProblems(plan, false, zero, afterExclude); len(p) != 0 {
		t.Fatalf("control: the delivery of the exclude chain must pass its own selection, got %v", p)
	}
	if p := rchSinkProblems(plan, true, zero, afterInclude); len(p) != 0 {
		t.Fatalf("control: the delivery of the include chain must pass its own selection, got %v", p)
	}
	jlWantProblem(t, "the include chain's delivery judged by the exclude selection", "selected=false) gained 1 message(s)", rchSinkProblems(plan, false, zero, afterInclude))
	jlWantProblem(t, "the exclude chain's delivery judged by the include selection", "selected=true) gained 0 message(s)", rchSinkProblems(plan, true, zero, afterExclude))
	{
		invalid := ""
		for _, a := range plan {
			if a.class == "invalid" {
				invalid = a.addr
				break
			}
		}
		w := afterExclude.clone()
		w.Msgs[invalid] = 1
		jlWantProblem(t, "a message to an invalid address", "a invalid verdict, selected=false) gained 1 message(s)", rchSinkProblems(plan, false, zero, w))
		w = afterExclude.clone()
		w.Rcpt[invalid] = m2wRcpt{Rejected: 1}
		jlWantProblem(t, "module 2 asked the sink about an invalid address (no message)", "selected=false) gained 0 message(s) at the sink and was answered at RCPT TO", rchSinkProblems(plan, false, zero, w))
		w = afterExclude.clone()
		for a := range rchSelectedSet(plan, false) {
			w.Msgs[a] = 2
			break
		}
		jlWantProblem(t, "a selected address sent twice", "gained 2 message(s)", rchSinkProblems(plan, false, zero, w))
		w = afterExclude.clone()
		for a := range rchSelectedSet(plan, false) {
			delete(w.Msgs, a)
			break
		}
		jlWantProblem(t, "a selected address never sent", "gained 0 message(s)", rchSinkProblems(plan, false, zero, w))
		w = afterExclude.clone()
		w.Msgs["d4bite-a-nobody@nowhere"] = 1
		jlWantProblem(t, "a message to an address of the run that no row owns", "no row of the source owns", rchSinkProblems(plan, false, zero, w))
		w = afterExclude.clone()
		w.Rcpt["d4bite-a-nobody@nowhere"] = m2wRcpt{Accepted: 1}
		jlWantProblem(t, "an RCPT TO answered for an address of the run that no row owns", "answered RCPT TO for an address", rchSinkProblems(plan, false, zero, w))
		before := afterExclude.clone() // a second chain over the same addresses: only the DELTA is judged
		if p := rchSinkProblems(plan, true, before, rchSinkView{Msgs: rchPlusOne(before.Msgs, rchSelectedSet(plan, true)), Rcpt: rchPlusOneRcpt(before.Rcpt, rchSelectedSet(plan, true))}); len(p) != 0 {
			t.Fatalf("control: a second chain over the same addresses is judged by what it gained, got %v", p)
		}
	}

	// the end of the test: the sink and module 2's own tables over every round
	{
		rounds := []rchRound{{plan, false}, {plan, true}, {small, false}}
		final := rchSinkView{Msgs: map[string]int{}, Rcpt: map[string]m2wRcpt{}}
		items, mjobs := map[string]int64{}, map[string]int64{}
		for addr, n := range rchExpectedSends(rounds) {
			if n > 0 {
				final.Msgs[addr] = n
				final.Rcpt[addr] = m2wRcpt{Accepted: n}
				items[addr], mjobs[addr] = int64(n), int64(n)
			}
		}
		for _, a := range append(append([]rchAddr{}, plan...), small...) {
			if a.class == "invalid" && rchExpectedSends(rounds)[a.addr] != 0 {
				t.Fatalf("an invalid address is expected to be sent to: the hand-written rule is broken")
			}
		}
		if p := rchCumulativeProblems(rounds, final); len(p) != 0 {
			t.Fatalf("control: the consistent end state must pass the cumulative sink check, got %v", p)
		}
		if p := rchModuleCountProblems(rounds, items, mjobs); len(p) != 0 {
			t.Fatalf("control: the consistent end state must pass the module 2 table check, got %v", p)
		}
		invalid, valid := "", ""
		for _, a := range plan {
			if a.class == "invalid" && invalid == "" {
				invalid = a.addr
			}
			if a.class == "valid" && valid == "" {
				valid = a.addr
			}
		}
		if rchExpectedSends(rounds)[valid] != 2 || rchExpectedSends(rounds)[invalid] != 0 {
			t.Fatalf("the owner's rule over the two chains of one source is %d send(s) to a valid address and %d to an invalid one, want 2 and 0", rchExpectedSends(rounds)[valid], rchExpectedSends(rounds)[invalid])
		}
		w := final.clone()
		w.Msgs[invalid] = 1
		jlWantProblem(t, "the end: a message to an invalid address", "(a invalid verdict) holds 1 message(s)", rchCumulativeProblems(rounds, w))
		w = final.clone()
		w.Rcpt[invalid] = m2wRcpt{Rejected: 1}
		jlWantProblem(t, "the end: an RCPT TO answer for an invalid address", "(a invalid verdict) holds 0 message(s)", rchCumulativeProblems(rounds, w))
		w = final.clone()
		w.Msgs[valid] = 1
		jlWantProblem(t, "the end: a valid address sent to once, not twice", "want 2 message(s) in all", rchCumulativeProblems(rounds, w))
		w = final.clone()
		w.Msgs["d4bite-a-nobody@nowhere"] = 1
		jlWantProblem(t, "the end: a message to an address no round owns", "no chained send owns", rchCumulativeProblems(rounds, w))
		it2, mj2 := map[string]int64{}, map[string]int64{}
		for k, v := range items {
			it2[k], mj2[k] = v, mjobs[k]
		}
		it2[invalid] = 1
		jlWantProblem(t, "the end: a module row written for an invalid address", "has 1 row(s) in public.job_items", rchModuleCountProblems(rounds, it2, mjobs))
		delete(it2, invalid)
		mj2[invalid] = 1
		jlWantProblem(t, "the end: a module job made for an invalid address", "and 1 job(s) in module 2's own table", rchModuleCountProblems(rounds, items, mj2))
		it3 := map[string]int64{}
		for k, v := range items {
			it3[k] = v
		}
		it3[valid] = 1
		jlWantProblem(t, "the end: a valid address with one module row for two sends", "want 2 of each", rchModuleCountProblems(rounds, it3, mjobs))
		it3 = map[string]int64{"d4bite-a-nobody@nowhere": 1}
		jlWantProblem(t, "the end: a module row of an address no round owns", "no chained send owns", rchModuleCountProblems(rounds, it3, mjobs))
	}

	// assertion 4: a refused confirm writes nothing
	refused := rchState{Status: jobs.StatusPreviewed, Available: 90, Reserved: 0}
	if p := rchNothingProblems(refused, 90, 0); len(p) != 0 {
		t.Fatalf("control: the state of a refused confirm must pass, got %v", p)
	}
	for _, c := range []struct {
		name, mention string
		mut           func(s *rchState)
	}{
		{"the job confirmed", "the job is confirmed", func(s *rchState) { s.Status = jobs.StatusConfirmed }},
		{"a reserve held", "holds a reserve", func(s *rchState) { s.ReserveHeld = true }},
		{"a ledger row written", "ledger row(s)", func(s *rchState) { s.LedgerRows = 2 }},
		{"a result written", "a refused confirm writes none", func(s *rchState) { s.Results = 4 }},
		{"a module row written", "public.job_items holds", func(s *rchState) { s.JobItems = 1 }},
		{"a module job made", "module 2 holds", func(s *rchState) { s.ModuleJobs = 1 }},
		{"the slot taken", "the slot must be free", func(s *rchState) { s.ActiveM2 = 1 }},
		{"a confirm key recorded", "carries a confirm key", func(s *rchState) { s.ConfirmKey = "k" }},
		{"a move to confirmed", "moved to confirmed", func(s *rchState) { s.Confirmed = 1 }},
		{"the balance moved", "balance is available", func(s *rchState) { s.Available = 86; s.Reserved = 4 }},
	} {
		w := refused
		c.mut(&w)
		jlWantProblem(t, "refused confirm: "+c.name, c.mention, rchNothingProblems(w, 90, 0))
	}
}

func rchPlusOne(m map[string]int, sel map[string]bool) map[string]int {
	out := map[string]int{}
	for k, v := range m {
		out[k] = v
	}
	for a := range sel {
		out[a]++
	}
	return out
}

func rchPlusOneRcpt(m map[string]m2wRcpt, sel map[string]bool) map[string]m2wRcpt {
	out := map[string]m2wRcpt{}
	for k, v := range m {
		out[k] = v
	}
	for a := range sel {
		c := out[a]
		c.Accepted++
		out[a] = c
	}
	return out
}
