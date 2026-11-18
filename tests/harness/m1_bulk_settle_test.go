//go:build harness

package harness

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	m1results "github.com/coldmail-platform/platform/internal/adapters/m1"
	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/modclient"
	"github.com/coldmail-platform/platform/internal/money"
)

// Phase 2 close-out D2, in the harness: a REAL bulk job on the REAL module 1 (built from the pinned source, running beside the platform, its drainer working
// the queue) is settled by the platform item by item, and the settlement is compared with the module's own report.
//
// What is real: module 1 (submit, drain, results, its own dedup and its own failure origin), the platform's money store over a real Postgres (grant, reserve at
// the dearest frozen price, one Settle per page, release) and the real internal/adapters/m1 adapter. What is not: the platform's job engine (Phase 4) and module
// 1's SMTP stage (no node in the stack, no DNS to the outside: the harness network is internal). The platform's money lives in a scratch database of its own
// (like every money test) so that a failed run leaves nothing in the live ledger; module 1 is the live one.
//
// THE ORACLE. The comparison does not call charge.Decide: that is what the settlement itself used, so it could not catch a wrong table. m1bOracle below is
// written out by hand, a switch over the module's own fields, from the charge table of the Phase 2 close-out (D1: Docs/phase-2-closeout-spec.md, "Charge-table
// changes") and internal/charge/rules.psv. If the two ever disagree this test is red and prints both sides.
//
// THE FIXTURE is chosen so that three things hold whatever the runner's DNS answers (the network has no route out, so every lookup of a well-formed address at
// a domain of ours fails, as unknown/dns_error with whatever failure origin module 1 attributes):
//   - three malformed rows: module 1 answers them from its own log row as invalid/invalid_syntax, which the table charges (M1-10);
//   - one address of one label (nodot) in three spellings: module 1's syntax layer fails it before any DNS lookup (a mailable domain has a dot), so the first
//     row is invalid/invalid_syntax, charged, and the two repeats are duplicate_of it, free;
//   - six addresses at six distinct domains: their outcome is the environment's (dns_error own, remote or something else), and the oracle follows the module's
//     report, whatever it is.

// m1bKeySeq makes every idempotency key of this file's submissions its own: module 1 scopes a key to the caller, and a repeat of a key with another request is a 409.
var m1bKeySeq atomic.Int64

// m1bKey is the Idempotency-Key of one submission of this test: the label, the clock and a counter (letters, digits, '.' and '-', well inside module 1's 128).
func m1bKey(label string) string {
	return fmt.Sprintf("m1b.%s.%d.%d", label, time.Now().UnixNano(), m1bKeySeq.Add(1))
}

const (
	m1bEnvURL = "HARNESS_M1_URL" // module 1's control plane on the private network
	m1bEnvKey = "HARNESS_M1_KEY" // its per-caller key: the one run.sh minted, seeded into verify.api_callers and masked in the log

	m1bPageLimit = 5 // results per page: the job has more rows than that, so it takes at least three pages
	m1bMinPages  = 3 // settle calls the page-by-page property demands
	m1bGrant     = 1000
)

// m1bRole says what a row of the fixture is for.
type m1bRole int

const (
	m1bPlain     m1bRole = iota // a well-formed address at a domain of its own
	m1bDupFirst                 // the first spelling of the repeated address
	m1bDupRepeat                // another spelling of it
	m1bMalformed                // module 1 cannot normalise it
)

var m1bFixture = []struct {
	address string
	role    m1bRole
}{
	{"ada@m1-live-a.example.test", m1bPlain},
	{"dup@nodot", m1bDupFirst},
	{"no-at-sign.example", m1bMalformed},
	{"bob@m1-live-b.example.test", m1bPlain},
	{"DUP@NoDot", m1bDupRepeat},
	{"two@@signs.example", m1bMalformed},
	{"cyd@m1-live-c.example.test", m1bPlain},
	{"@no-local-part.example", m1bMalformed},
	{"dee@m1-live-d.example.test", m1bPlain},
	{" dup@nodot ", m1bDupRepeat},
	{"eli@m1-live-e.example.test", m1bPlain},
	{"fay@m1-live-f.example.test", m1bPlain},
}

func m1bLabel(ordinal int) string { return "row-" + strconv.Itoa(ordinal) }

func m1bItems() []m1results.Item {
	items := make([]m1results.Item, len(m1bFixture))
	for i, f := range m1bFixture {
		items[i] = m1results.Item{ItemID: m1bLabel(i), Address: f.address}
	}
	return items
}

func m1bOrdinals(role m1bRole) []int {
	var out []int
	for i, f := range m1bFixture {
		if f.role == role {
			out = append(out, i)
		}
	}
	return out
}

// m1bObserve is the observer of the contract drift session of the m1node binary (zz_m1_node_contract_drift_test.go sets it in init): nil everywhere else, where no session exists.
var m1bObserve func(modclient.Observation)

func m1bClient(t *testing.T) *m1results.Adapter {
	t.Helper()
	base := m3lBaseURL(t, m1bEnvURL)
	key := strings.TrimSpace(requireEnv(t, m1bEnvKey))
	c, err := m1results.New(base, config.Secret(key), nil, modclient.Options{Timeout: 30 * time.Second, Observe: m1bObserve})
	if err != nil {
		t.Fatalf("cannot build the module 1 client from %s / %s: %v", m1bEnvURL, m1bEnvKey, err) // New's errors never carry the URL or the key
	}
	c.PageLimit = m1bPageLimit
	c.Poll = 500 * time.Millisecond
	return c
}

// ---- the oracle ----------------------------------------------------------------------------------------------------------------------------------------

// m1bDecision is what the oracle says a result costs.
type m1bDecision struct {
	Kind string // "charge", "free" or "unmapped"
	Unit string // the unit, for a charge
	Why  string
}

func (d m1bDecision) String() string {
	if d.Kind == "charge" {
		return "charge " + d.Unit
	}
	return d.Kind
}

func m1bCharge1(unit, why string) m1bDecision { return m1bDecision{"charge", unit, why} }
func m1bFree(why string) m1bDecision          { return m1bDecision{"free", "", why} }
func m1bUnmapped(why string) m1bDecision      { return m1bDecision{"unmapped", "", why} }

// m1bOracle is the charge table as module 1's report reads against it, written by hand and NOT through internal/charge. First match wins, in this order:
//
//	a repeat of an earlier row (duplicate_of)            free: the first row of the address carries the charge (P-07)
//	answered from the cache (flags.from_cache)           charge m1_cache (M1-01)
//	reason external_verified                             charge m1_vendor (M1-02)
//	valid/accepted, valid/mailbox_full                   charge m1_own
//	invalid/{no_mx, null_mx, spam_trap, mailbox_not_found, historical_bounce, invalid_syntax}   charge m1_own (positive proof; invalid_syntax is M1-10)
//	risky/{disposable, provider_no_smtp, catch_all, non_public_mx}                              charge m1_own
//	risky/plausible with a capacity or wiring token (no_eligible_node, audit_red, warmup_cap, rate_limited, smtp_unwired)   free: OUR failure
//	risky/plausible with no token                        charge m1_own (the cheap layers' answer)
//	unknown/<any reason> by signals.failure_origin       remote: charge m1_own; own or undetermined: free; absent: unmapped (the module broke its contract)
//
// Anything else is unmapped, and the test refuses an unmapped result, so a status the oracle does not know is a red test and a line to add by hand.
func m1bOracle(r m1results.Row) m1bDecision {
	switch {
	case r.DuplicateOf != nil:
		return m1bFree("a repeat of ordinal " + strconv.Itoa(*r.DuplicateOf) + ": the first row of the address carries the charge")
	case r.FromCache:
		return m1bCharge1("m1_cache", "answered from the cache")
	case r.Reason == "external_verified":
		return m1bCharge1("m1_vendor", "the vendor fallback")
	case r.Status == "unknown": // whatever its reason (greylisted, dns_error, smtp_timeout, smtp_error, connection_failed, smtp_blocked, unknown_error): who caused it decides
		switch r.FailureOrigin {
		case "remote":
			return m1bCharge1("m1_own", "the recipient's side caused it")
		case "own", "undetermined":
			return m1bFree("an unknown caused by us or not provable (" + r.FailureOrigin + ")")
		}
		return m1bUnmapped("an unknown with no failure_origin (" + strconv.Quote(r.FailureOrigin) + ")")
	}
	switch r.Status + "/" + r.Reason {
	case "valid/accepted", "valid/mailbox_full":
		return m1bCharge1("m1_own", "a probed answer")
	case "invalid/no_mx", "invalid/null_mx", "invalid/spam_trap", "invalid/mailbox_not_found", "invalid/historical_bounce", "invalid/invalid_syntax":
		return m1bCharge1("m1_own", "positive proof")
	case "risky/disposable", "risky/provider_no_smtp", "risky/catch_all", "risky/non_public_mx":
		return m1bCharge1("m1_own", "a settled answer")
	case "risky/plausible":
		switch r.SkipReason {
		case "":
			return m1bCharge1("m1_own", "the cheap layers' answer")
		case "no_eligible_node", "audit_red", "warmup_cap", "rate_limited", "smtp_unwired":
			return m1bFree("our own capacity or wiring failure (" + r.SkipReason + ")")
		}
		return m1bUnmapped("risky/plausible with the skip token " + strconv.Quote(r.SkipReason))
	}
	return m1bUnmapped("status/reason " + r.Status + "/" + r.Reason + " is not in the oracle")
}

// m1bCharge is one charge row of the ledger.
type m1bCharge struct {
	ItemRef string
	Unit    string
	Cost    int
	Units   int
}

// m1bCompare is the item-by-item comparison of a settlement with the module's report. It returns every disagreement (sorted, so a failure is stable), and
// nothing when the two agree: for each item of the report, one ledger charge keyed by its item_id with the oracle's unit at the frozen price and one unit if
// the oracle says charge, none if it says free or unmapped; and no ledger row under a key the report does not know (an ordinal, an address, a guess).
func m1bCompare(report []m1results.Row, ledger []m1bCharge, prices map[string]int) []string {
	var problems []string
	byRef := map[string][]m1bCharge{}
	for _, c := range ledger {
		byRef[c.ItemRef] = append(byRef[c.ItemRef], c)
	}
	known := map[string]int{}
	for _, r := range report {
		known[r.ItemID]++
		if known[r.ItemID] > 1 {
			problems = append(problems, fmt.Sprintf("the report names item_id %q more than once", r.ItemID))
			continue
		}
		d := m1bOracle(r)
		got := byRef[r.ItemID]
		if d.Kind != "charge" {
			if len(got) != 0 {
				problems = append(problems, fmt.Sprintf("item %q (ordinal %d): the report makes it %s (%s) but the ledger charged it %d time(s) as %v", r.ItemID, r.Ordinal, d.Kind, d.Why, len(got), got))
			}
			continue
		}
		if len(got) != 1 {
			problems = append(problems, fmt.Sprintf("item %q (ordinal %d): the report makes it a charge of %s (%s) but the ledger holds %d charge row(s) for it", r.ItemID, r.Ordinal, d.Unit, d.Why, len(got)))
			continue
		}
		c := got[0]
		if c.Unit != d.Unit {
			problems = append(problems, fmt.Sprintf("item %q (ordinal %d): the report implies unit %s, the ledger charged unit %s", r.ItemID, r.Ordinal, d.Unit, c.Unit))
		}
		if want, ok := prices[d.Unit]; !ok || c.Cost != want {
			problems = append(problems, fmt.Sprintf("item %q (ordinal %d): charged %d credit(s) per unit, the frozen price of %s is %d (listed: %v)", r.ItemID, r.Ordinal, c.Cost, d.Unit, want, ok))
		}
		if c.Units != 1 {
			problems = append(problems, fmt.Sprintf("item %q (ordinal %d): the charge row is for %d unit(s), a charge is one item", r.ItemID, r.Ordinal, c.Units))
		}
	}
	for ref, cs := range byRef {
		if known[ref] == 0 {
			problems = append(problems, fmt.Sprintf("the ledger holds %d charge row(s) under item_ref %q, which is no item_id of the report", len(cs), ref))
		}
	}
	sort.Strings(problems)
	return problems
}

// m1bPageProperty checks that a job was settled page by page: pages holds the ordinals of each settle call, in order. At least minPages calls and at least
// ceil(total/limit); every call between 1 and limit rows; every ordinal 0..total-1 exactly once in all. One settlement at the end is a single call and fails it.
func m1bPageProperty(pages [][]int, total, limit, minPages int) []string {
	var p []string
	if len(pages) < minPages {
		p = append(p, fmt.Sprintf("%d settle call(s), want at least %d: a job settled in so few calls was not settled page by page", len(pages), minPages))
	}
	if want := (total + limit - 1) / limit; len(pages) < want {
		p = append(p, fmt.Sprintf("%d settle call(s) for %d rows at %d per page, want at least %d", len(pages), total, limit, want))
	}
	seen := map[int]int{}
	for i, pg := range pages {
		if len(pg) == 0 || len(pg) > limit {
			p = append(p, fmt.Sprintf("settle call %d held %d row(s), want 1..%d", i, len(pg), limit))
		}
		for _, o := range pg {
			seen[o]++
		}
	}
	for o := 0; o < total; o++ {
		if seen[o] != 1 {
			p = append(p, fmt.Sprintf("ordinal %d was settled %d time(s), want exactly once", o, seen[o]))
		}
	}
	for o := range seen {
		if o < 0 || o >= total {
			p = append(p, fmt.Sprintf("ordinal %d is outside the job's 0..%d", o, total-1))
		}
	}
	sort.Strings(p)
	return p
}

// ---- reading what happened ----------------------------------------------------------------------------------------------------------------------------

func (e *mnyEnv) m1bLedger(job string) []m1bCharge {
	e.t.Helper()
	rows, err := e.admin.Query(testCtx(e.t), `SELECT item_ref, unit, unit_cost, units FROM platform.ledger WHERE job_id = $1 AND kind = 'charge' ORDER BY id`, job)
	if err != nil {
		e.t.Fatalf("cannot read the ledger of job %s: %v", job, err)
	}
	defer rows.Close()
	var out []m1bCharge
	for rows.Next() {
		var c m1bCharge
		if err := rows.Scan(&c.ItemRef, &c.Unit, &c.Cost, &c.Units); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		e.t.Fatal(err)
	}
	return out
}

// m1bReport re-reads the whole results list of a COMPLETE job through the client, independently of the settle loop (its own page size, its own walk), and
// returns the module's report. It fails the test unless the job is complete and the report is every input row exactly once, in order, each with the label it
// was submitted with: that is what the platform relies on when it keys a charge by item_id.
func m1bReport(t *testing.T, ctx context.Context, m1 *m1results.Adapter, jobID string, total int) []m1results.Row {
	t.Helper()
	js, err := m1.Status(ctx, jobID)
	if err != nil {
		t.Fatalf("cannot read module 1's job status: %v", err)
	}
	if !js.Complete || js.Counts.Total != total || js.Counts.Settled != total || js.Counts.Pending != 0 {
		t.Fatalf("module 1's job is not complete as the report is read: %+v (want complete, total = settled = %d, pending 0)", js, total)
	}
	var report []m1results.Row
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 10*total {
			t.Fatalf("the report walk did not end after %d pages: the cursor does not advance", pages)
		}
		rows, next, err := m1.Results(ctx, jobID, cursor, 3)
		if err != nil {
			t.Fatalf("cannot read module 1's report: %v", err)
		}
		report = append(report, rows...)
		if next == "" {
			break
		}
		cursor = next
	}
	if len(report) != total {
		t.Fatalf("module 1's report has %d result(s) for %d input rows", len(report), total)
	}
	for i, r := range report {
		if r.Ordinal != i || r.ItemID != m1bLabel(i) {
			t.Fatalf("result %d of the report is ordinal %d item_id %q, want ordinal %d item_id %q: a result must attribute to exactly one input row, in order", i, r.Ordinal, r.ItemID, i, m1bLabel(i))
		}
	}
	return report
}

// m1bTable prints the report and the oracle's decision for each row: what a failure message shows.
func m1bTable(report []m1results.Row) string {
	var b strings.Builder
	b.WriteString("  ord  item_id   dup   status/reason                    cache skip              origin        -> oracle\n")
	for _, r := range report {
		fmt.Fprintf(&b, "  %-4d %-9s %-5s %-32s %-5t %-17s %-13s -> %s\n", r.Ordinal, r.ItemID, orDash(m1bDup(r)), r.Status+"/"+r.Reason, r.FromCache, orDash(r.SkipReason), orDash(r.FailureOrigin), m1bOracle(r))
	}
	return b.String()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// m1bDup renders a row's duplicate_of for a message: its ordinal, or "" when it has none.
func m1bDup(r m1results.Row) string {
	if r.DuplicateOf == nil {
		return ""
	}
	return strconv.Itoa(*r.DuplicateOf)
}

// m1bPage is one settle call of the drain.
type m1bPage struct {
	rows    []m1results.Row
	items   []money.SettleItem
	res     money.SettleResult
	pending int // module 1's pending count read just before the settle; -1 when that read failed
}

// ---- the test ----------------------------------------------------------------------------------------------------------------------------------------

// TestM1Live_BulkJobSettlesItemByItem (close-out D2): see the file comment. It fails if a result cannot be attributed to its row, if a charge is keyed by
// anything but module 1's item_id, if the ledger and the module's report disagree on any item (the oracle is hand-written), if the three spellings of one
// address are charged other than once, if settling a page again charges anything, if the books drift, or if the job was not settled page by page. The
// subtests are its controls: a settlement keyed by ordinal is caught, the comparison is red on every corruption of the report, and the page property is red
// on a settlement at the end.
func TestM1Live_BulkJobSettlesItemByItem(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	m1 := m1bClient(t)
	e := newMnyEnv(t)
	items := m1bItems()
	total := len(items)

	// ---- the money side: a customer with credits, a job, a reserve at the dearest frozen price ----
	if _, err := e.store.Grant(ctx, money.GrantReq{UserID: mnyUserA, Module: "m1", Amount: m1bGrant, AdminID: mnyAdminA, Reason: "module 1 live bulk settlement", Key: "live-grant-a"}); err != nil {
		t.Fatalf("grant: %v", err)
	}
	job := e.newJob(mnyUserA, "m1")
	rsv := e.reserve(job, total)
	e.setStatus(job, "running")
	if rsv.Reserved != int64(total)*int64(rsv.UnitCost) {
		t.Fatalf("the reserve holds %d credits for %d units at %d", rsv.Reserved, total, rsv.UnitCost)
	}
	t.Logf("money: granted %d credits; reserved %d for %d row(s) at %d per unit (%s, the dearest of the frozen snapshot %v)", m1bGrant, rsv.Reserved, total, rsv.UnitCost, rsv.Unit, rsv.Prices)

	// ---- module 1: submit ----
	submitted, err := m1.SubmitRaw(ctx, items, m1bKey("settle"))
	if err != nil {
		t.Fatalf("submitting the bulk job to module 1 failed: %v\n(a 400 here means module 1 does not take the `items` shape: is the pinned module 1 at the commit that has A1, the per-item identity?)", err)
	}
	m1Job, counts := submitted.JobID, submitted.Counts
	if want := (m1results.SubmitCounts{Total: total, Enqueued: total - len(m1bOrdinals(m1bMalformed)) - len(m1bOrdinals(m1bDupRepeat)), Duplicate: len(m1bOrdinals(m1bDupRepeat)), Malformed: len(m1bOrdinals(m1bMalformed))}); counts != want {
		t.Fatalf("module 1's 202 counts are %+v, want %+v (total = enqueued + duplicate + malformed, over input rows)", counts, want)
	}
	t.Logf("module 1 accepted the job: %+v", counts)

	// ---- drain, settling EACH PAGE as it arrives ----
	var pages []m1bPage
	drainCtx, cancelDrain := context.WithTimeout(ctx, 10*time.Minute)
	defer cancelDrain()
	stats, err := m1.Drain(drainCtx, m1Job, func(rows []m1results.Row) error {
		settle, err := m1results.ToSettleItems(rows)
		if err != nil {
			return err
		}
		pending := -1
		if js, jerr := m1.Status(drainCtx, m1Job); jerr == nil {
			pending = js.Counts.Pending
		}
		res, err := e.store.Settle(drainCtx, money.SettleReq{JobID: job, Items: settle})
		if err != nil {
			return err
		}
		pages = append(pages, m1bPage{rows: rows, items: settle, res: res, pending: pending})
		return nil
	})
	if err != nil {
		diag := "module 1's job status could not be read"
		fresh, cancelFresh := context.WithTimeout(context.Background(), 20*time.Second)
		if js, jerr := m1.Status(fresh, m1Job); jerr == nil {
			diag = fmt.Sprintf("module 1 now says: status %q, complete %t, total %d, settled %d, pending %d", js.Status, js.Complete, js.Counts.Total, js.Counts.Settled, js.Counts.Pending)
		}
		cancelFresh()
		t.Fatalf("draining and settling the job failed: %v\n%s\nsubmit counts %+v; %d page(s) settled before the failure, %d row(s) delivered", err, diag, counts, len(pages), stats.Rows)
	}

	// ---- the module's own report, read again, whole ----
	report := m1bReport(t, ctx, m1, m1Job, total)
	if got := m1bFlatten(pages); !reflect.DeepEqual(got, report) {
		t.Fatalf("the rows the drain delivered are not the rows the finished job lists (a complete job's listing is immutable):\ndrain delivered:\n%s\nfinal report:\n%s", m1bTable(got), m1bTable(report))
	}

	// ---- (a) the ledger against the oracle, item by item ----
	ledger := e.m1bLedger(job)
	prices := rsv.Prices
	table := m1bTable(report)
	for _, r := range report {
		if d := m1bOracle(r); d.Kind == "unmapped" {
			t.Fatalf("module 1 reported a result the charge table cannot classify (ordinal %d: %s): an unknown without its failure_origin, or a status the oracle does not know.\n%s", r.Ordinal, d.Why, table)
		}
	}
	if problems := m1bCompare(report, ledger, prices); len(problems) != 0 {
		t.Fatalf("the settlement does not match module 1's own report (%d disagreement(s)):\n  %s\nthe module's report and the oracle's decisions:\n%s\nthe ledger's charges: %v\nper-page settle results: %s", len(problems), strings.Join(problems, "\n  "), table, ledger, m1bResults(pages))
	}
	var wantCharged, wantFree int
	var wantCredits int64
	for _, r := range report {
		if d := m1bOracle(r); d.Kind == "charge" {
			wantCharged++
			wantCredits += int64(prices[d.Unit])
		} else {
			wantFree++
		}
	}
	if len(ledger) != wantCharged {
		t.Fatalf("the ledger holds %d charge row(s), the report implies %d\n%s", len(ledger), wantCharged, table)
	}
	if wantCharged == 0 || wantFree == 0 {
		t.Fatalf("the job produced %d charged and %d free item(s): it must hold both, or the comparison proves too little\n%s", wantCharged, wantFree, table)
	}
	for _, o := range m1bOrdinals(m1bMalformed) {
		if r := report[o]; r.Status != "invalid" || r.Reason != "invalid_syntax" || r.DuplicateOf != nil {
			t.Fatalf("the malformed row %d came back as %s/%s (duplicate_of %s), want invalid/invalid_syntax and never a duplicate\n%s", o, r.Status, r.Reason, m1bDup(r), table)
		}
		if got := m1bChargesOf(ledger, report[o].ItemID); len(got) != 1 || got[0].Unit != "m1_own" {
			t.Fatalf("the malformed row %d is charged %v, want one m1_own charge (M1-10)\n%s", o, got, table)
		}
	}

	// ---- (b) three spellings of one address are charged exactly once ----
	first := m1bOrdinals(m1bDupFirst)[0]
	group := append([]int{first}, m1bOrdinals(m1bDupRepeat)...)
	if r := report[first]; r.DuplicateOf != nil || r.Status != "invalid" || r.Reason != "invalid_syntax" {
		t.Fatalf("the first spelling of the repeated address came back as %s/%s duplicate_of %s: the fixture address (one label, no dot) must fail module 1's syntax layer without a DNS lookup, as invalid/invalid_syntax, so that its charge is deterministic; update the fixture if module 1 changed\n%s", r.Status, r.Reason, m1bDup(r), table)
	}
	charged := 0
	for _, o := range group {
		r := report[o]
		if o != first && (r.DuplicateOf == nil || *r.DuplicateOf != first) {
			t.Fatalf("module 1 did not mark ordinal %d as a repeat of ordinal %d (duplicate_of %s): the spellings of one address are checked once and each repeat names the first row\n%s", o, first, m1bDup(r), table)
		}
		if r.Status != report[first].Status || r.Reason != report[first].Reason {
			t.Fatalf("the spellings of one address got different verdicts (%s/%s and %s/%s): one check, one verdict\n%s", report[first].Status, report[first].Reason, r.Status, r.Reason, table)
		}
		charged += len(m1bChargesOf(ledger, r.ItemID))
	}
	if charged != 1 || len(m1bChargesOf(ledger, report[first].ItemID)) != 1 {
		t.Fatalf("the %d spellings of one address were charged %d time(s) in total (the first row: %d), want exactly once, on the first row\n%s", len(group), charged, len(m1bChargesOf(ledger, report[first].ItemID)), table)
	}

	// ---- the settle results add up ----
	var sumCharged, sumFree int
	var sumCredits int64
	for _, pg := range pages {
		sumCharged += pg.res.Charged
		sumFree += pg.res.Free
		sumCredits += pg.res.Credits
		if pg.res.AlreadySettled != 0 || pg.res.NotFinal != 0 || len(pg.res.Unmapped) != 0 {
			t.Fatalf("a first settlement of a page reported already-settled %d, not-final %d, unmapped %v\n%s", pg.res.AlreadySettled, pg.res.NotFinal, pg.res.Unmapped, table)
		}
	}
	if sumCharged != wantCharged || sumFree != wantFree || sumCredits != wantCredits {
		t.Fatalf("the settle calls reported charged %d free %d credits %d, the report implies charged %d free %d credits %d\n%s", sumCharged, sumFree, sumCredits, wantCharged, wantFree, wantCredits, table)
	}

	// ---- (c) settling every page again charges nothing, and the books are exactly as they were ----
	before := e.state()
	for i, pg := range pages {
		again, err := e.store.Settle(ctx, money.SettleReq{JobID: job, Items: pg.items})
		if err != nil {
			t.Fatalf("settling page %d a second time: %v", i, err)
		}
		var pc, pf int
		for _, r := range pg.rows {
			if m1bOracle(r).Kind == "charge" {
				pc++
			} else {
				pf++
			}
		}
		if again.Charged != 0 || again.Credits != 0 || again.AlreadySettled != pc || again.Free != pf || len(again.Unmapped) != 0 {
			t.Fatalf("settling page %d again: charged %d credits %d already-settled %d free %d unmapped %v; want charged 0, credits 0, already-settled %d (every item the ledger charged), free %d\n%s", i, again.Charged, again.Credits, again.AlreadySettled, again.Free, again.Unmapped, pc, pf, table)
		}
	}
	e.requireSame("settling every page a second time", before)
	e.requireBal("after the settlement", mnyUserA, "m1", m1bGrant-rsv.Reserved, rsv.Reserved-wantCredits)

	// ---- the end of the job: release, and the numbers are grant - charged and reserve - charged - released ----
	e.setStatus(job, "succeeded")
	rel, err := e.store.Release(ctx, job)
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if rel.Released != rsv.Reserved-wantCredits {
		t.Fatalf("the release returned %d, want reserve %d - charged %d = %d", rel.Released, rsv.Reserved, wantCredits, rsv.Reserved-wantCredits)
	}
	e.requireBal("after the release", mnyUserA, "m1", m1bGrant-wantCredits, 0)
	if got := e.rows(`SELECT reserved, charged, released FROM platform.reserves WHERE reserve_id = $1`, rsv.ReserveID); len(got) != 1 ||
		got[0] != fmt.Sprintf("%d|%d|%d", rsv.Reserved, wantCredits, rsv.Reserved-wantCredits) {
		t.Fatalf("platform.reserves holds %v for the job's reserve, want reserved %d, charged %d, released %d", got, rsv.Reserved, wantCredits, rsv.Reserved-wantCredits)
	}
	e.requireViewsEmpty("after the live bulk settlement (balance_drift and reserve_overdrawn)")

	// ---- (d) settled page by page ----
	var shape [][]int
	pendingPages := 0
	for _, pg := range pages {
		shape = append(shape, ordinalsOfRows(pg.rows))
		if pg.pending > 0 {
			pendingPages++
		}
	}
	if problems := m1bPageProperty(shape, total, m1bPageLimit, m1bMinPages); len(problems) != 0 {
		t.Fatalf("the job was not settled page by page: %s\nsettle calls (ordinals per call): %v", strings.Join(problems, "; "), shape)
	}
	if len(pages) != stats.Pages || stats.Rows != total {
		t.Fatalf("%d settle call(s) but the drain reports %d page(s) and %d row(s) for a job of %d", len(pages), stats.Pages, stats.Rows, total)
	}
	// NOT ASSERTED: that a page was settled while module 1 still had rows pending. Whether the module has settled everything before the first page is read is a
	// race against its drainer (two seconds between polls, a DNS lookup per row), so it is printed, not asserted; the property that IS asserted is the number of
	// calls and that they partition the job.

	// ---- what the run saw ----
	dist := map[string]int{}
	for _, r := range report {
		dist[fmt.Sprintf("%s/%s origin=%s skip=%s -> %s", r.Status, r.Reason, orDash(r.FailureOrigin), orDash(r.SkipReason), m1bOracle(r))]++
	}
	var lines []string
	for k, n := range dist {
		lines = append(lines, fmt.Sprintf("%d x %s", n, k))
	}
	sort.Strings(lines)
	t.Logf("NOTE the distribution this runner saw (the oracle follows module 1's own fields, so any origin is fine): %d charged (%d credits), %d free; %s", wantCharged, wantCredits, wantFree, strings.Join(lines, "; "))
	t.Logf("NOTE settled page by page: %d settle call(s) of at most %d row(s) for %d row(s) (the drain waited %d time(s), %d row(s) were caught up behind the cursor); %d of the %d page(s) were settled while module 1 still had rows pending", len(pages), m1bPageLimit, total, stats.Waits, stats.CatchUp, pendingPages, len(pages))
	for _, line := range strings.Split(strings.TrimRight(table, "\n"), "\n") { // one log line per row: run.sh prints the lines that carry this file's name
		t.Logf("NOTE report | %s", line)
	}

	// ---- (e) the controls ----
	t.Run("a settlement keyed by ordinal is caught", func(t *testing.T) {
		et := e.at(t)
		et.fund(mnyUserB, "m1", m1bGrant)
		jobB := et.newJob(mnyUserB, "m1")
		rsvB := et.reserve(jobB, total)
		et.setStatus(jobB, "running")
		for start := 0; start < len(report); start += m1bPageLimit {
			chunk := report[start:min(start+m1bPageLimit, len(report))]
			settle, err := m1results.ToSettleItems(chunk)
			if err != nil {
				t.Fatal(err)
			}
			for i := range settle {
				settle[i].ItemRef = strconv.Itoa(chunk[i].Ordinal) // the mistake: a position instead of the module's id
			}
			et.settle(jobB, settle...)
		}
		ledgerB := et.m1bLedger(jobB)
		// the totals look right: an aggregate check would pass
		var credits int64
		for _, c := range ledgerB {
			credits += int64(c.Cost)
		}
		if len(ledgerB) != len(ledger) || credits != wantCredits {
			t.Fatalf("control setup: the mis-keyed settlement holds %d charge(s) for %d credits, the right one %d for %d: the point of this control is that the totals agree", len(ledgerB), credits, len(ledger), wantCredits)
		}
		problems := m1bCompare(report, ledgerB, rsvB.Prices)
		text := strings.Join(problems, "\n")
		if len(problems) == 0 {
			t.Fatal("the item-by-item comparison found nothing wrong with a settlement keyed by ordinal: it cannot catch a mis-keyed settlement")
		}
		if !strings.Contains(text, "holds 0 charge row(s) for it") || !strings.Contains(text, "is no item_id of the report") {
			t.Fatalf("the comparison is red but not for the reasons it must name (a chargeable item with no charge under its item_id, and charges under keys that are no item_id):\n%s", text)
		}
	})

	t.Run("the comparison is red on a corrupted report", func(t *testing.T) { m1bCheckComparisonIsRed(t, report, ledger, prices) })

	t.Run("the page property is red when the job is settled at the end", func(t *testing.T) { m1bCheckPagePropertyIsRed(t, shape, total) })

	// the books once more, after the controls wrote a second user's reserve and charges
	e.requireViewsEmpty("after the controls")
	if n := e.requireReservesMatchLedger("after the live bulk settlement and its controls"); n != 2 {
		t.Fatalf("%d reserve(s) compared with the ledger, want 2 (the job's and the control's)", n)
	}
}

// ---- small helpers of the test -----------------------------------------------------------------------------------------------------------------------

func m1bChargesOf(ledger []m1bCharge, itemRef string) []m1bCharge {
	var out []m1bCharge
	for _, c := range ledger {
		if c.ItemRef == itemRef {
			out = append(out, c)
		}
	}
	return out
}

func ordinalsOfRows(rows []m1results.Row) []int {
	out := make([]int, len(rows))
	for i, r := range rows {
		out[i] = r.Ordinal
	}
	return out
}

// m1bFlatten returns every row the drain delivered, in ordinal order.
func m1bFlatten(pages []m1bPage) []m1results.Row {
	var all []m1results.Row
	for _, pg := range pages {
		all = append(all, pg.rows...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Ordinal < all[j].Ordinal })
	return all
}

func m1bResults(pages []m1bPage) string {
	var parts []string
	for i, pg := range pages {
		parts = append(parts, fmt.Sprintf("[%d: ordinals %v charged %d credits %d free %d unmapped %d]", i, ordinalsOfRows(pg.rows), pg.res.Charged, pg.res.Credits, pg.res.Free, len(pg.res.Unmapped)))
	}
	return strings.Join(parts, " ")
}

// m1bCheckComparisonIsRed is the control of m1bCompare: the uncorrupted report agrees with the ledger, and each corruption of a copy of the report (a charged
// item called a repeat, a free item called a fresh answer, a unit changed to the cache's or the vendor's, an item missing, two results with one item_id, an
// item renamed, a price that is not the frozen one) makes the comparison red, with the reason it must name. A comparison that cannot fail proves nothing.
func m1bCheckComparisonIsRed(t *testing.T, report []m1results.Row, ledger []m1bCharge, prices map[string]int) {
	t.Helper()
	if problems := m1bCompare(report, ledger, prices); len(problems) != 0 {
		t.Fatalf("control: the uncorrupted report disagrees with the ledger: %v", problems)
	}
	firstCharged, firstFree := -1, -1
	for i, r := range report {
		d := m1bOracle(r)
		switch d.Kind {
		case "charge":
			if firstCharged < 0 && r.DuplicateOf == nil && d.Unit == "m1_own" { // an m1_own charge: the corruptions below move it to another unit
				firstCharged = i
			}
		case "free":
			if firstFree < 0 {
				firstFree = i
			}
		}
	}
	if firstCharged < 0 || firstFree < 0 {
		t.Fatalf("setup: no m1_own charged row (%d) or no free row (%d) to corrupt", firstCharged, firstFree)
	}
	corrupt := func(mutate func(rows []m1results.Row) []m1results.Row) []m1results.Row {
		cp := make([]m1results.Row, len(report))
		copy(cp, report)
		return mutate(cp)
	}
	zero := 0
	cases := []struct {
		name, mention string
		rows          []m1results.Row
	}{
		{"a charged item the report now calls a repeat", "but the ledger charged it", corrupt(func(r []m1results.Row) []m1results.Row {
			r[firstCharged].DuplicateOf = &zero
			return r
		})},
		{"a free item the report now calls a fresh answer", "holds 0 charge row(s) for it", corrupt(func(r []m1results.Row) []m1results.Row {
			r[firstFree].DuplicateOf = nil
			r[firstFree].Status, r[firstFree].Reason, r[firstFree].FailureOrigin, r[firstFree].SkipReason = "valid", "accepted", "", ""
			return r
		})},
		{"a charged item the report now says came from the cache", "the report implies unit m1_cache", corrupt(func(r []m1results.Row) []m1results.Row {
			r[firstCharged].FromCache = true
			return r
		})},
		{"a charged item the report now says is the vendor's", "the report implies unit m1_vendor", corrupt(func(r []m1results.Row) []m1results.Row {
			r[firstCharged].Reason = "external_verified"
			return r
		})},
		{"a charged item missing from the report", "is no item_id of the report", corrupt(func(r []m1results.Row) []m1results.Row {
			return append(r[:firstCharged:firstCharged], r[firstCharged+1:]...)
		})},
		{"two results with one item_id", "more than once", corrupt(func(r []m1results.Row) []m1results.Row {
			r[firstFree].ItemID = r[firstCharged].ItemID
			return r
		})},
		{"a charged item renamed", "is no item_id of the report", corrupt(func(r []m1results.Row) []m1results.Row {
			r[firstCharged].ItemID = "renamed-" + r[firstCharged].ItemID
			return r
		})},
	}
	for _, c := range cases {
		problems := m1bCompare(c.rows, ledger, prices)
		if len(problems) == 0 {
			t.Errorf("%s: the comparison found nothing wrong", c.name)
			continue
		}
		if !strings.Contains(strings.Join(problems, "\n"), c.mention) {
			t.Errorf("%s: the comparison is red but does not say %q:\n%s", c.name, c.mention, strings.Join(problems, "\n"))
		}
	}
	// a price that is not the frozen one, and a ledger row for a unit of the wrong size
	wrong := map[string]int{}
	for u, p := range prices {
		wrong[u] = p + 1
	}
	if problems := m1bCompare(report, ledger, wrong); len(problems) == 0 {
		t.Error("the comparison accepted a ledger priced differently from the frozen snapshot")
	}
	twice := append(append([]m1bCharge(nil), ledger...), ledger[0])
	if problems := m1bCompare(report, twice, prices); len(problems) == 0 {
		t.Error("the comparison accepted an item charged twice")
	}
}

// m1bCheckPagePropertyIsRed is the control of m1bPageProperty: the real settlement satisfies it, and a job settled in one call at the end, a page bigger than
// the limit, a row never settled, a row settled twice and an empty call each break it, with the reason it must name.
func m1bCheckPagePropertyIsRed(t *testing.T, shape [][]int, total int) {
	t.Helper()
	if problems := m1bPageProperty(shape, total, m1bPageLimit, m1bMinPages); len(problems) != 0 {
		t.Fatalf("control: the real settlement fails the property: %v", problems)
	}
	all := make([]int, total)
	for i := range all {
		all[i] = i
	}
	cases := []struct {
		name, mention string
		pages         [][]int
	}{
		{"one settlement at the end", "settle call(s), want at least", [][]int{all}},
		{"a page bigger than the limit", "want 1..", [][]int{all[:m1bPageLimit+1], all[m1bPageLimit+1:]}},
		{"a row never settled", "was settled 0 time(s)", [][]int{all[0:5], all[5:9], all[9 : total-1]}},
		{"a row settled twice", "was settled 2 time(s)", [][]int{all[0:5], all[4:9], all[9:]}},
		{"an empty call", "want 1..", [][]int{all[0:5], {}, all[5:10], all[10:]}},
	}
	for _, c := range cases {
		problems := m1bPageProperty(c.pages, total, m1bPageLimit, m1bMinPages)
		if len(problems) == 0 {
			t.Errorf("%s: the property found nothing wrong", c.name)
			continue
		}
		if !strings.Contains(strings.Join(problems, "\n"), c.mention) {
			t.Errorf("%s: red, but not saying %q: %v", c.name, c.mention, problems)
		}
	}
}
