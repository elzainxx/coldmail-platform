package m3_test

import (
	"fmt"
	"testing"
)

// The recordings are what the real module answered when the harness drove it, and the file a recording was filed under is the harness's classification of the answer, not a promise about
// what the answer holds (the README says which recordings are not what their name suggests). A test that needs a particular KIND of answer (a whole campaign in one page, a listing with
// accounts in it, the quote of a count) therefore picks the recording by what it holds, read from the file, and fails naming the files when none holds it: it does not rely on a name.

// firstWith returns the first of names whose document satisfies ok; none is a failed test that says what was looked for.
func firstWith(t testing.TB, what string, names []string, ok func(d map[string]any) bool) string {
	t.Helper()
	for _, n := range names {
		if ok(doc(t, n)) {
			return n
		}
	}
	t.Fatalf("none of the recordings %v holds %s", names, what)
	return ""
}

// terminalCampaign is a recorded campaign report that is the whole campaign in one page (a cursor of null) with items in it: the page a walk reads once.
func terminalCampaign(t testing.TB) string {
	t.Helper()
	return firstWith(t, "a whole campaign in one page", []string{"report.campaign.page.json", "report.campaign.last_page.json", "report.campaign.empty.json"}, func(d map[string]any) bool {
		return optStr(d, "items.next_cursor") == nil && length(t, d, "items.results") > 0
	})
}

// emptyCampaignBody is the answer for a campaign the module knows nothing about (a stranger's, a typo): the terminal recorded page with its items, its totals and its progress taken
// out. The recordings hold no such answer (the harness drove no empty campaign), so it is built from one, which is also the control: the recording itself is a campaign.
func emptyCampaignBody(t testing.TB) []byte {
	t.Helper()
	return edit(t, readFixture(t, terminalCampaign(t)), func(m map[string]any) {
		sub(t, m, "items")["results"] = []any{}
		sub(t, m, "items")["next_cursor"] = nil
		for k := range sub(t, m, "totals") {
			sub(t, m, "totals")[k] = 0
		}
		m["totals_by_account"] = []any{}
		sub(t, m, "progress")["terminal"] = 0
		sub(t, m, "progress")["fraction_complete"] = 0
		sub(t, m, "progress")["is_complete"] = false
	})
}

// populatedList is a recorded listing of accounts with at least one account in it (whatever its cursor); terminalList is one that is also the whole listing (a cursor of null).
func populatedList(t testing.TB) string {
	t.Helper()
	return firstWith(t, "a listing with accounts in it", []string{"accounts.list.page.json", "accounts.list.empty.json", "accounts.list.last_page.json"}, func(d map[string]any) bool {
		return length(t, d, "items") > 0
	})
}

func terminalList(t testing.TB) string {
	t.Helper()
	return firstWith(t, "a whole listing of accounts in one page", []string{"accounts.list.empty.json", "accounts.list.last_page.json", "accounts.list.page.json"}, func(d map[string]any) bool {
		return optStr(d, "next_cursor") == nil && length(t, d, "items") > 0
	})
}

// quoteCount is the recipient count a recorded quote was made for: the count to ask for when that recording is what answers.
func quoteCount(t testing.TB, name string) int64 { return asInt(t, doc(t, name), "recipient_count") }

// strangerQuoteBody is the module's quote for a tenant it has no accounts of: the recorded feasible quote with its verdict, its capacity and its projection changed to what the module's
// code answers for zero accounts (not feasible, no_eligible_accounts, nothing eligible, no duration), for the same count.
func strangerQuoteBody(t testing.TB) []byte {
	t.Helper()
	return edit(t, readFixture(t, "quote.feasible.json"), func(m map[string]any) {
		m["feasible"] = false
		m["reasons"] = []any{map[string]any{"code": "no_eligible_accounts"}}
		m["warnings"] = []any{}
		c := sub(t, m, "capacity")
		c["eligible_accounts"] = 0
		c["daily_capacity_remaining"] = 0
		delete(c, "estimated_duration_sec")
		delete(c, "estimate_basis")
	})
}

// bothCapsQuote is the module's quote for a count that breaks the campaign size cap while the queue depth cap is full, and the count it was made for. The recorded infeasible quote holds ONE reason
// (the queue depth cap: the real module's answer of that shape, see the README), so a quote of both caps is built from it: the count is one above the recorded campaign size cap, every reason
// the recording holds is kept with that count as the one requested, and the campaign size cap's reason (code campaign_cap, its cause, the cap's own number as the limit, nothing held yet) is put in
// front of them, which is the order the module lists the causes in (the campaign size cap, then the queue depth cap). The recording itself is the control: it is the same body without the first reason.
func bothCapsQuote(t testing.TB) ([]byte, int64) {
	t.Helper()
	rec := doc(t, "quote.infeasible.json")
	n := asInt(t, rec, "caps.campaign_size_cap") + 1
	campaign := map[string]any{"code": "campaign_cap", "cause": "campaign_cap", "cap_name": "campaign_size_cap", "limit": asInt(t, rec, "caps.campaign_size_cap"), "requested": n, "current": 0}
	body := edit(t, readFixture(t, "quote.infeasible.json"), func(m map[string]any) {
		m["recipient_count"] = n
		reasons := []any{campaign}
		for _, r := range m["reasons"].([]any) {
			rm := r.(map[string]any)
			if rm["cause"] == "campaign_cap" {
				t.Fatalf("the recorded quote already names the campaign size cap: a quote of both caps needs no built reason")
			}
			if _, ok := rm["requested"]; ok {
				rm["requested"] = n
			}
			reasons = append(reasons, rm)
		}
		m["reasons"] = reasons
	})
	return body, n
}

// docDetails lists an error body's details as "field/issue", in the module's order, read from the file (the independent parse a Check holds the adapter's Details to).
func docDetails(t testing.TB, d any) []string {
	t.Helper()
	v, ok := lookup(d, "error.details")
	if !ok {
		return nil
	}
	var out []string
	for i := range v.([]any) {
		out = append(out, asStr(t, d, fmt.Sprintf("error.details[%d].field", i))+"/"+asStr(t, d, fmt.Sprintf("error.details[%d].issue", i)))
	}
	return out
}

// namesUsername reports whether an error body names smtp_username in its details: the module's mark of a duplicate account.
func namesUsername(t testing.TB, d any) bool {
	t.Helper()
	for _, x := range docDetails(t, d) {
		if x == "smtp_username/conflict" {
			return true
		}
	}
	return false
}
