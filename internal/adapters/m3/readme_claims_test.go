package m3_test

import (
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/adapters/sendapi/sendapitest"
)

// The README of testdata says, of a dozen recordings, that what the file holds is not what its name suggests (a contract case name is a shape family: the real module's answer of that shape can
// hold another state). Those claims rot the way the first version of them did (a re-recording changed what the files hold and the sentences stayed), so each is held to the file here: every
// claim is a predicate over the parsed document, the README must name the file, and the predicate must hold. The controls: every predicate is false for a document that says nothing (none is
// true unconditionally), and a recording made to say otherwise (a bounced item that says sent, a quarantined account that says healthy) fails its claim.
func TestReadmeClaimsAboutFilesWhoseContentDiffersFromTheirNameAreTrue(t *testing.T) {
	text := string(sendapitest.Read(t, "testdata", "README.md"))
	type check = func(d map[string]any) bool
	eqStr := func(p, want string) check {
		return func(d map[string]any) bool {
			v, ok := lookup(d, p)
			s, isStr := v.(string)
			return ok && isStr && s == want
		}
	}
	eqNum := func(p string, want float64) check {
		return func(d map[string]any) bool {
			v, ok := lookup(d, p)
			f, isNum := v.(float64)
			return ok && isNum && f == want
		}
	}
	eqBool := func(p string, want bool) check {
		return func(d map[string]any) bool {
			v, ok := lookup(d, p)
			b, isBool := v.(bool)
			return ok && isBool && b == want
		}
	}
	lenIs := func(p string, want int) check {
		return func(d map[string]any) bool {
			v, ok := lookup(d, p)
			a, isArr := v.([]any)
			return ok && isArr && len(a) == want
		}
	}
	isNull := func(p string) check {
		return func(d map[string]any) bool { v, ok := lookup(d, p); return ok && v == nil }
	}
	nonEmpty := func(p string) check {
		return func(d map[string]any) bool {
			v, ok := lookup(d, p)
			s, isStr := v.(string)
			return ok && isStr && s != ""
		}
	}
	missing := func(p string) check { return func(d map[string]any) bool { _, ok := lookup(d, p); return !ok } }
	allOf := func(cs ...check) check {
		return func(d map[string]any) bool {
			for _, c := range cs {
				if !c(d) {
					return false
				}
			}
			return true
		}
	}
	claims := []struct {
		file  string
		holds check
	}{
		{"report.job.sent.json", allOf(eqStr("state", "bounced"), eqStr("error_class", "bounce_permanent"))},
		{"report.campaign.last_page.json", allOf(lenIs("items.results", 2), nonEmpty("items.next_cursor"), eqNum("totals.total", 5), eqNum("items.page_size", 2), eqStr("items.results[0].state", "queued"), eqStr("items.results[1].state", "queued"))},
		{"report.campaign.empty.json", allOf(lenIs("items.results", 1), nonEmpty("items.next_cursor"), eqNum("totals.total", 45), eqNum("totals.sent", 45), eqNum("items.page_size", 1))},
		{"report.campaign.page.json", allOf(lenIs("items.results", 12), isNull("items.next_cursor"))},
		{"accounts.list.last_page.json", allOf(lenIs("items", 0), isNull("next_cursor"))},
		{"accounts.list.empty.json", allOf(lenIs("items", 1), isNull("next_cursor"), eqBool("items[0].status.quarantined", true))},
		{"accounts.list.page.json", allOf(lenIs("items", 2), nonEmpty("next_cursor"))},
		{"accounts.list.error.validation_failed.json", eqStr("error.details[0].field", "cursor")},
		{"accounts.list.error.cursor_malformed.json", eqStr("error.details[0].field", "user_id")},
		{"accounts.get.ok.json", allOf(eqBool("status.quarantined", true), eqStr("status.quarantine_reason", "auth_failure"), nonEmpty("status.quarantined_at"), eqStr("verification.state", "verified"), eqStr("from_alignment.state", "aligned"))},
		{"accounts.create.created.json", allOf(eqStr("verification.state", "verified"), eqStr("from_alignment.state", "unaligned_warned"), nonEmpty("from_alignment.acknowledged_at"))},
		{"accounts.create.created_failed.json", allOf(eqStr("verification.state", "failed"), eqStr("verification.code", "verify_connect_failed"), eqStr("verification.smtp_response", ""), eqStr("from_alignment.state", "unaligned_warned"), nonEmpty("from_alignment.acknowledged_at"))},
		{"accounts.test.ok.json", allOf(eqStr("verification.state", "failed"), eqStr("verification.code", "verify_tls_failed"), eqStr("verification.smtp_response", ""), eqStr("from_alignment.state", "aligned"))},
		{"accounts.test.failed.json", allOf(eqStr("verification.code", "verify_auth_failed"), nonEmpty("verification.smtp_response"), eqStr("from_alignment.state", "aligned"), missing("from_alignment.error"))},
		{"accounts.test.indeterminate.json", allOf(eqStr("verification.code", "verify_tls_failed"), eqStr("from_alignment.state", "indeterminate"), eqStr("from_alignment.code", "from_dns_unavailable"))},
		{"quote.ok.json", allOf(eqNum("recipient_count", 0), eqBool("feasible", true), eqNum("caps.campaign_size_cap", 2), lenIs("warnings", 0), eqNum("capacity.estimated_duration_sec", 0))},
		{"quote.feasible.json", allOf(eqNum("recipient_count", 100), eqBool("feasible", true), eqStr("warnings[0].code", "long_duration"), eqNum("warnings[0].estimated_duration_sec", 170), eqNum("warnings[0].threshold_sec", 60))},
		{"quote.infeasible.json", allOf(eqNum("recipient_count", 1), eqBool("feasible", false), lenIs("reasons", 1), eqStr("reasons[0].cause", "queue_cap"), eqNum("reasons[0].current", 3), eqNum("reasons[0].limit", 3), eqNum("caps.queue_depth_cap", 3), eqNum("caps.campaign_size_cap", 50000))},
		{"run.error.conflict.json", allOf(lenIs("error.details", 1), eqStr("error.details[0].code", "queue_cap"), eqNum("error.details[0].current", 3), eqNum("error.details[0].limit", 3))},
		{"accounts.create.error.duplicate.json", allOf(lenIs("error.details", 1), eqStr("error.details[0].field", "smtp_username"))},
		{"accounts.create.error.conflict.json", allOf(eqStr("error.code", "conflict"), missing("error.details"))},
		{"accounts.events.last_page.json", allOf(lenIs("items", 3), isNull("next_cursor"), eqStr("items[0].action", "created"), eqStr("items[1].action", "verified"), eqStr("items[2].action", "quarantine"),
			eqStr("items[0].id", norm1), eqStr("items[0].account_id", norm2), eqStr("items[0].user_id", norm3))},
	}
	if len(claims) < 20 {
		t.Fatalf("%d claims: the subject shrank", len(claims))
	}
	for _, c := range claims {
		if !strings.Contains(text, "`"+c.file+"`") {
			t.Errorf("the README no longer names %s, which this test holds a claim about", c.file)
			continue
		}
		if !c.holds(doc(t, c.file)) {
			t.Errorf("the README says of %s what the file does not hold: read the file, then correct the README and this claim", c.file)
		}
		if c.holds(map[string]any{}) {
			t.Errorf("the claim about %s is true of a document that says nothing: it cannot fail", c.file)
		}
	}
	// the controls, concretely: a recording made to say otherwise fails its claim
	broken := doc(t, "report.job.sent.json")
	broken["state"] = "sent"
	if claims[0].holds(broken) {
		t.Fatal("a bounced item that says sent still holds the claim")
	}
	healthy := doc(t, "accounts.get.ok.json")
	sub(t, healthy, "status")["quarantined"] = false
	delete(sub(t, healthy, "status"), "quarantine_reason")
	for _, c := range claims {
		if c.file == "accounts.get.ok.json" && c.holds(healthy) {
			t.Fatal("a healthy account still holds the claim that the recording is a quarantined one")
		}
	}
}
