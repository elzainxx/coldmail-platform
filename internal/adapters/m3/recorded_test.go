package m3_test

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/adapters/m3"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi/sendapitest"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// This file is what lets the unit tests run over RECORDED bodies: what a test expects of a recording it reads from the recording itself (a path into the file, a count of its items), or from a
// constant named after the normalisation (norm1, norm2), never from a literal that was true of an older, hand-written file. Each helper below compares what the adapter returned with the
// document it was served, field by field, so that a recording that changes is followed by the assertion and a decoder that drops or invents a field is caught.

// doc reads a fixture as a generic JSON document (numbers as float64).
func doc(t testing.TB, name string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(sendapitest.Read(t, fixtures, name), &m); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return m
}

var segRe = regexp.MustCompile(`^([A-Za-z0-9_]+)((?:\[[0-9]+\])*)$`)

// lookup walks a path ("items.results[0].state") through a decoded document; ok is false when any step is missing.
func lookup(v any, path string) (any, bool) {
	for _, seg := range strings.Split(path, ".") {
		m := segRe.FindStringSubmatch(seg)
		if m == nil {
			return nil, false
		}
		obj, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}
		if v, ok = obj[m[1]]; !ok {
			return nil, false
		}
		for _, ix := range regexp.MustCompile(`\[([0-9]+)\]`).FindAllStringSubmatch(m[2], -1) {
			n, _ := strconv.Atoi(ix[1])
			a, ok := v.([]any)
			if !ok || n >= len(a) {
				return nil, false
			}
			v = a[n]
		}
	}
	return v, true
}

// must is lookup that fails the test when the path is missing: a recording that no longer has the key a test reads is a finding, not a skip.
func must(t testing.TB, d any, path string) any {
	t.Helper()
	v, ok := lookup(d, path)
	if !ok {
		t.Fatalf("the recorded body has no %s", path)
	}
	return v
}

func asStr(t testing.TB, d any, path string) string {
	t.Helper()
	s, ok := must(t, d, path).(string)
	if !ok {
		t.Fatalf("%s is not a string", path)
	}
	return s
}

func asInt(t testing.TB, d any, path string) int64 {
	t.Helper()
	f, ok := must(t, d, path).(float64)
	if !ok {
		t.Fatalf("%s is not a number", path)
	}
	return int64(f)
}

// optStr is an optional string of a recording: nil when the key is absent or null.
func optStr(d any, path string) *string {
	v, ok := lookup(d, path)
	if !ok || v == nil {
		return nil
	}
	s, _ := v.(string)
	return &s
}

func optTime(t testing.TB, d any, path string) *time.Time {
	t.Helper()
	s := optStr(d, path)
	if s == nil {
		return nil
	}
	ts, err := time.Parse(time.RFC3339Nano, *s)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return &ts
}

func length(t testing.TB, d any, path string) int {
	t.Helper()
	a, ok := must(t, d, path).([]any)
	if !ok {
		t.Fatalf("%s is not an array", path)
	}
	return len(a)
}

// vocabSet is a closed set the contracts name, by name: the one list the adapter's switch is held equal to.
func vocabSet(t testing.TB, name string) []string {
	t.Helper()
	for _, c := range m3.Contracts() {
		if v, ok := c.Vocab[name]; ok {
			return v
		}
	}
	t.Fatalf("no contract names the vocabulary %s", name)
	return nil
}

// mapped is what the adapter makes of a module's word: itself when the closed set has it, "unrecognised" otherwise.
func mapped(w string, set []string) string {
	if slices.Contains(set, w) {
		return w
	}
	return "unrecognised"
}

func same[T comparable](t testing.TB, what string, got, want T) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: got %v, the recording says %v", what, got, want)
	}
}

func samePtr(t testing.TB, what string, got, want *string) {
	t.Helper()
	switch {
	case got == nil && want == nil:
	case got == nil || want == nil:
		t.Fatalf("%s: got %v, the recording says %v", what, got, want)
	case *got != *want:
		t.Fatalf("%s: got %q, the recording says %q", what, *got, *want)
	}
}

func sameTime(t testing.TB, what string, got, want *time.Time) {
	t.Helper()
	switch {
	case got == nil && want == nil:
	case got == nil || want == nil:
		t.Fatalf("%s: got %v, the recording says %v", what, got, want)
	case !got.Equal(*want):
		t.Fatalf("%s: got %v, the recording says %v", what, got, want)
	}
}

// accountMatches holds an Account to the object it was decoded from (d is the account object itself, or one element of a list).
func accountMatches(t testing.TB, a m3.Account, d any) {
	t.Helper()
	same(t, "id", a.ID, asStr(t, d, "id"))
	same(t, "user_id", a.UserID, asStr(t, d, "user_id"))
	same(t, "smtp_host", a.SMTPHost, asStr(t, d, "smtp_host"))
	same(t, "smtp_port", int64(a.SMTPPort), asInt(t, d, "smtp_port"))
	same(t, "smtp_username", a.SMTPUsername, asStr(t, d, "smtp_username"))
	same(t, "from_address", a.FromAddress, asStr(t, d, "from_address"))
	samePtr(t, "from_name", a.FromName, optStr(d, "from_name"))
	same(t, "has_credential", a.HasCredential, must(t, d, "has_credential").(bool))
	same(t, "paused", a.Paused, must(t, d, "status.paused").(bool))
	pause := ""
	if p := optStr(d, "status.pause_reason"); p != nil {
		pause = mapped(*p, vocabSet(t, "pause_reason"))
	}
	same(t, "pause_reason", a.PauseReason, pause)
	same(t, "quarantined", a.Quarantined, must(t, d, "status.quarantined").(bool))
	quar := ""
	if p := optStr(d, "status.quarantine_reason"); p != nil {
		quar = mapped(*p, vocabSet(t, "quarantine_reason"))
	}
	same(t, "quarantine_reason", a.QuarantineReason, quar)
	sameTime(t, "quarantined_at", a.QuarantinedAt, optTime(t, d, "status.quarantined_at"))
	same(t, "disabled", a.Disabled, must(t, d, "status.disabled").(bool))

	same(t, "verification.state", string(a.Verification.State), mapped(asStr(t, d, "verification.state"), vocabSet(t, "credential_verification_state")))
	sameTime(t, "verified_at", a.Verification.VerifiedAt, optTime(t, d, "verification.verified_at"))
	sameTime(t, "verify_at", a.Verification.VerifyAt, optTime(t, d, "verification.verify_at"))
	code := ""
	if c := optStr(d, "verification.code"); c != nil {
		code = mapped(*c, vocabSet(t, "verify_code"))
	}
	same(t, "verification.code", string(a.Verification.Code), code)
	samePtr(t, "verification.smtp_response", a.Verification.SMTPResponse, optStr(d, "verification.smtp_response"))

	same(t, "from_alignment.state", string(a.FromAlignment.State), mapped(asStr(t, d, "from_alignment.state"), vocabSet(t, "from_verification_state")))
	dm := ""
	if p := optStr(d, "from_alignment.dmarc_policy"); p != nil {
		dm = *p
	}
	same(t, "dmarc_policy", a.FromAlignment.DMARCPolicy, dm)
	sameTime(t, "checked_at", a.FromAlignment.CheckedAt, optTime(t, d, "from_alignment.checked_at"))
	samePtr(t, "from_alignment.message", a.FromAlignment.Message, optStr(d, "from_alignment.message"))
	sameTime(t, "acknowledged_at", a.FromAlignment.AcknowledgedAt, optTime(t, d, "from_alignment.acknowledged_at"))
	same(t, "daily_limit", int64(a.DailyLimit), asInt(t, d, "daily_limit"))
	sameTime(t, "created_at", &a.CreatedAt, optTime(t, d, "created_at"))
	sameTime(t, "updated_at", &a.UpdatedAt, optTime(t, d, "updated_at"))
}

// classOf is what the shared body says each state means for settlement (queued waits, assigned and sending are in flight, every other state is final).
func classOf(state string) sendapi.Class {
	switch state {
	case "queued":
		return sendapi.ClassPending
	case "assigned", "sending":
		return sendapi.ClassInFlight
	}
	return sendapi.ClassTerminal
}

// itemMatches holds an Item to the job it was decoded from: a single report (single true: it names job_id) or one element of a campaign report (job_item_id). Every optional key of the
// recording is nil in the Item exactly when the recording omits it or sends null, and carries the recording's value otherwise.
func itemMatches(t testing.TB, it sendapi.Item, d any, single bool) {
	t.Helper()
	if single {
		same(t, "job_id", it.JobID, asStr(t, d, "job_id"))
	} else {
		same(t, "job_item_id", it.JobItemID, asStr(t, d, "job_item_id"))
	}
	state := asStr(t, d, "state")
	same(t, "state", it.State, state)
	same(t, "class", it.Class, classOf(state))
	same(t, "attempt_count", it.AttemptCount, asInt(t, d, "attempt_count"))
	if it.Reason != nil {
		t.Fatalf("module 3 has no reason key, got %q", *it.Reason)
	}
	samePtr(t, "error_class", it.ErrorClass, optStr(d, "error_class"))
	samePtr(t, "provider_message_id", it.ProviderMessageID, optStr(d, "provider_message_id"))
	sameTime(t, "completed_at", it.CompletedAt, optTime(t, d, "completed_at"))
	samePtr(t, "smtp_response", it.SMTPResponse, optStr(d, "smtp_response"))
	sameTime(t, "failed_at", it.FailedAt, optTime(t, d, "failed_at"))
	samePtr(t, "account_id", it.AccountID, optStr(d, "account_id"))
	samePtr(t, "account_username", it.AccountUsername, optStr(d, "account_username"))
}

// pageMatches holds a CampaignPage to the campaign report it was decoded from.
func pageMatches(t testing.TB, p sendapi.CampaignPage, d any) {
	t.Helper()
	same(t, "campaign_id", p.CampaignID, asStr(t, d, "campaign_id"))
	same(t, "dispatch_paused", p.DispatchPaused, must(t, d, "dispatch_paused").(bool))
	same(t, "page_size", int64(p.PageSize), asInt(t, d, "items.page_size"))
	next := ""
	if n := optStr(d, "items.next_cursor"); n != nil {
		next = *n
	}
	same(t, "next_cursor", p.NextCursor, next)
	for k, got := range map[string]int64{
		"total": p.Totals.Total, "sent": p.Totals.Sent, "bounced": p.Totals.Bounced, "failed": p.Totals.Failed, "in_flight": p.Totals.InFlight, "pending": p.Totals.Pending,
		"waiting_for_capacity": p.Totals.WaitingForCapacity,
	} {
		same(t, "totals."+k, got, asInt(t, d, "totals."+k))
	}
	same(t, "totals.cancelled", p.Totals.Cancelled, 0)
	same(t, "progress.terminal", p.Progress.Terminal, asInt(t, d, "progress.terminal"))
	same(t, "progress.is_complete", p.Progress.IsComplete, must(t, d, "progress.is_complete").(bool))
	if f := must(t, d, "progress.fraction_complete").(float64); p.Progress.FractionComplete != f {
		t.Fatalf("fraction_complete: got %v, the recording says %v", p.Progress.FractionComplete, f)
	}
	same(t, "items", len(p.Items), length(t, d, "items.results"))
	for i, it := range p.Items {
		itemMatches(t, it, must(t, d, fmt.Sprintf("items.results[%d]", i)), false)
	}
	same(t, "totals_by_account", len(p.ByAccount), length(t, d, "totals_by_account"))
	for i, a := range p.ByAccount {
		base := fmt.Sprintf("totals_by_account[%d].", i)
		same(t, base+"account_id", a.AccountID, asStr(t, d, base+"account_id"))
		same(t, base+"account_username", a.AccountUsername, asStr(t, d, base+"account_username"))
		same(t, base+"sent", a.Sent, asInt(t, d, base+"sent"))
		same(t, base+"bounced", a.Bounced, asInt(t, d, base+"bounced"))
		same(t, base+"failed", a.Failed, asInt(t, d, base+"failed"))
		same(t, base+"in_flight", a.InFlight, asInt(t, d, base+"in_flight"))
	}
}

// bulkRows is the number of rows the recorded bulk answer has: a batch of that many accounts is what the recording answers.
func bulkRows(t testing.TB) int { return length(t, doc(t, "accounts.bulk.ok.json"), "results") }

// bulkMatches holds a BulkResult to the bulk answer it was decoded from, row by row.
func bulkMatches(t testing.TB, r m3.BulkResult, d any) {
	t.Helper()
	same(t, "request_id", r.RequestID, asStr(t, d, "request_id"))
	same(t, "rows", len(r.Rows), length(t, d, "results"))
	created := 0
	for i, x := range r.Rows {
		base := fmt.Sprintf("results[%d].", i)
		same(t, base+"row", int64(x.Row), asInt(t, d, base+"row"))
		isCreated := asStr(t, d, base+"status") == "created"
		same(t, base+"created", x.Created, isCreated)
		if isCreated {
			created++
			same(t, base+"account_id", x.AccountID, asStr(t, d, base+"account_id"))
			same(t, base+"verification", string(x.Verification), mapped(asStr(t, d, base+"verification"), vocabSet(t, "credential_verification_state")))
			code := ""
			if c := optStr(d, base+"code"); c != nil {
				code = mapped(*c, vocabSet(t, "verify_code"))
			}
			same(t, base+"code", string(x.VerifyCode), code)
			samePtr(t, base+"smtp_response", x.SMTPResponse, optStr(d, base+"smtp_response"))
			al := ""
			if a := optStr(d, base+"from_alignment_state"); a != nil {
				al = mapped(*a, vocabSet(t, "from_verification_state"))
			}
			same(t, base+"from_alignment_state", string(x.Alignment), al)
			samePtr(t, base+"from_alignment_message", x.AlignmentMessage, optStr(d, base+"from_alignment_message"))
			continue
		}
		same(t, base+"account_id", x.AccountID, "")
		code := ""
		if c := optStr(d, base+"code"); c != nil {
			code = *c
		}
		same(t, base+"code", string(x.Reason), mapped(code, vocabSet(t, "bulk_reject_code")))
		var want []m3.FieldIssue
		if v, ok := lookup(d, base+"details"); ok {
			for j := range v.([]any) {
				want = append(want, m3.FieldIssue{Field: asStr(t, d, fmt.Sprintf("%sdetails[%d].field", base, j)), Issue: asStr(t, d, fmt.Sprintf("%sdetails[%d].issue", base, j))})
			}
		}
		if !slices.Equal(x.Fields, want) {
			t.Fatalf("%sdetails: got %v, the recording says %v", base, x.Fields, want)
		}
	}
	if created == 0 || created == len(r.Rows) {
		t.Fatalf("the recording is not a mixed batch (%d created of %d): a bulk contract that holds one kind of row cannot show that a rejection is told from a creation", created, len(r.Rows))
	}
}

// edit decodes a body, lets f change the document and encodes it again: how a test builds a variant of a RECORDED body (a key the recording lacks, a row the recording did not catch)
// without a second file that can drift from the first.
func edit(t testing.TB, body []byte, f func(m map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	f(m)
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// sub returns the object at a path of a decoded document (creating nothing): the place an injected key goes.
func sub(t testing.TB, m map[string]any, path string) map[string]any {
	t.Helper()
	v, ok := lookup(m, path)
	if !ok {
		t.Fatalf("no %s to edit", path)
	}
	o, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s is not an object", path)
	}
	return o
}

// ---- the recordings are numbered the way the tests assume --------------------------------------------------------------------------

var normUUID = regexp.MustCompile(`^00000000-0000-4000-8000-[0-9]{12}$`)

// The tests name the ids of the account fixtures and of the campaign reports after contractcheck.Normalise (norm1, norm2). This holds every recorded file to that: each account file names
// its account first and its tenant second, every account of a listing has the one tenant, a campaign report names its campaign first; every id of every fixture has the normalised or the
// synthetic spelling (no real uuid); and the timestamps of the recordings are the normalised instant. The control: a file numbered otherwise is caught by the same check.
func TestRecordedFixturesCarryTheNormalisedIDs(t *testing.T) {
	idProblems := func(name string, d map[string]any) []string {
		var p []string
		check := func(path, want string) {
			if v, ok := lookup(d, path); !ok || v != want {
				p = append(p, fmt.Sprintf("%s: %s is %v, want %s", name, path, v, want))
			}
		}
		switch {
		case strings.HasPrefix(name, "accounts.get.ok"), strings.HasPrefix(name, "accounts.create.created"):
			check("id", norm1)
			check("user_id", norm2)
		case name == "accounts.list.page.json" || name == "accounts.list.empty.json":
			n, _ := lookup(d, "items")
			for i := range n.([]any) {
				check(fmt.Sprintf("items[%d].user_id", i), norm2)
			}
			check("items[0].id", norm1)
		case strings.HasPrefix(name, "report.campaign.page"), strings.HasPrefix(name, "report.campaign.empty"), strings.HasPrefix(name, "report.campaign.last_page"):
			check("campaign_id", norm1)
		}
		return p
	}
	recorded := []string{
		"accounts.get.ok.json", "accounts.create.created.json", "accounts.create.created_failed.json", "accounts.list.page.json", "accounts.list.empty.json",
		"report.campaign.page.json", "report.campaign.empty.json",
	}
	handWritten := []string{"accounts.list.last_page.json", "accounts.events.page.json", "report.campaign.last_page.json"}
	for _, name := range append(slices.Clone(recorded), handWritten...) {
		if p := idProblems(name, doc(t, name)); len(p) != 0 {
			t.Fatalf("%v", p)
		}
	}
	// the events of the hand-written page name the same tenant and account
	ev := doc(t, "accounts.events.page.json")
	for i := 0; i < length(t, ev, "items"); i++ {
		same(t, "events user_id", asStr(t, ev, fmt.Sprintf("items[%d].user_id", i)), userA)
		same(t, "events account_id", asStr(t, ev, fmt.Sprintf("items[%d].account_id", i)), acct1)
	}
	// every uuid-shaped value of a recording is a normalised one, and its timestamps are the normalised instant
	for _, name := range recordedFixtures {
		body := string(sendapitest.Read(t, fixtures, name))
		for _, m := range regexp.MustCompile(`"[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}"`).FindAllString(body, -1) {
			if !normUUID.MatchString(strings.Trim(m, `"`)) {
				t.Fatalf("%s: %s is not a normalised id", name, m)
			}
		}
		for _, m := range regexp.MustCompile(`"20[0-9]{2}-[0-9]{2}-[0-9]{2}T[^"]*"`).FindAllString(body, -1) {
			if !strings.HasPrefix(m, `"2026-01-01T00:00:00`) {
				t.Fatalf("%s: %s is not the normalised instant", name, m)
			}
		}
	}
	// control: the same check names a file that is numbered otherwise
	renumbered := doc(t, "accounts.get.ok.json")
	renumbered["user_id"] = norm3
	if p := idProblems("accounts.get.ok.json", renumbered); len(p) != 1 || !strings.Contains(p[0], "user_id") {
		t.Fatalf("a renumbered file was not caught: %v", p)
	}
}

// ---- what the recordings do not hold, built from the recordings --------------------------------------------------------------------

// The recordings hold the module's ordinary answers. Four keys the adapter reads are ones the module sends only sometimes and the harness did not provoke: an acknowledgment instant on an
// account, a provider message id on a failed item's report, the test button's `error` key, a warned verdict. Each is injected into a RECORDED body, once with a good value (accepted and
// carried, the control) and once with a value of the wrong type or an unreadable instant (refused, naming the key).
func TestKeysTheRecordingsLackAreReadAndRefusedWhenBroken(t *testing.T) {
	type tc struct {
		name    string
		fixture string
		inject  func(m map[string]any, v any)
		good    any
		bad     any
		badKey  string
		call    func(t testing.TB, url string) (any, error)
		check   func(t testing.TB, v any)
	}
	client := func(t testing.TB, url string) *m3.Client { return clientAt(t, url, sendapitest.Token(t)) }
	acct := func(path, key string) func(m map[string]any, v any) {
		return func(m map[string]any, v any) { sub(t, m, path)[key] = v }
	}
	for _, c := range []tc{
		{
			name: "from_alignment.acknowledged_at", fixture: "accounts.create.created_failed.json", inject: acct("from_alignment", "acknowledged_at"),
			good: "2026-01-01T00:00:00Z", bad: "when", badKey: "from_alignment.acknowledged_at",
			call: func(t testing.TB, url string) (any, error) {
				return client(t, url).CreateAccount(ctxT(t), newAcct(t, 1))
			},
			check: func(t testing.TB, v any) {
				sameTime(t, "acknowledged_at", v.(m3.Account).FromAlignment.AcknowledgedAt, ptrTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)))
			},
		},
		{
			name: "from_alignment.message", fixture: "accounts.get.ok.json", inject: acct("from_alignment", "message"),
			good: "The From address matches the sending server.", bad: 3, badKey: "from_alignment.message",
			call: func(t testing.TB, url string) (any, error) { return client(t, url).GetAccount(ctxT(t), acct1) },
			check: func(t testing.TB, v any) {
				samePtr(t, "message", v.(m3.Account).FromAlignment.Message, str("The From address matches the sending server."))
			},
		},
		{
			name: "from_alignment.last_attempt_at", fixture: "accounts.get.ok.json", inject: acct("from_alignment", "last_attempt_at"),
			good: "2026-01-01T00:00:00Z", bad: 5, badKey: "from_alignment.last_attempt_at",
			call:  func(t testing.TB, url string) (any, error) { return client(t, url).GetAccount(ctxT(t), acct1) },
			check: func(t testing.TB, v any) {},
		},
		{
			name: "provider_message_id", fixture: "report.job.sent.json", inject: func(m map[string]any, v any) { m["provider_message_id"] = v },
			good: "AAAAAAAAAAAAAAAAA", bad: 7, badKey: "provider_message_id",
			call: func(t testing.TB, url string) (any, error) { return client(t, url).ReportJob(ctxT(t), jobItem) },
			check: func(t testing.TB, v any) {
				samePtr(t, "provider_message_id", v.(sendapi.Item).ProviderMessageID, str("AAAAAAAAAAAAAAAAA"))
			},
		},
		{
			name: "from_alignment.error", fixture: "accounts.test.failed.json", inject: acct("from_alignment", "error"),
			good: "from_gate_write_error", bad: 5, badKey: "from_alignment.error",
			call: func(t testing.TB, url string) (any, error) { return client(t, url).TestAccount(ctxT(t), acct1, false) },
			check: func(t testing.TB, v any) {
				r := v.(m3.TestResult)
				// the module's internal slug becomes a boolean and none of its text is carried
				if !r.Alignment.NotStored || strings.Contains(reflectDump(r), "from_gate_write_error") {
					t.Fatalf("result = %+v", r)
				}
			},
		},
		{
			name: "from_alignment.state warned", fixture: "accounts.test.failed.json", inject: acct("from_alignment", "state"),
			good: "unaligned_warned", bad: 5, badKey: "from_alignment.state",
			call:  func(t testing.TB, url string) (any, error) { return client(t, url).TestAccount(ctxT(t), acct1, true) },
			check: func(t testing.TB, v any) { same(t, "state", v.(m3.TestResult).Alignment.State, m3.AlignmentWarned) },
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			base := sendapitest.Read(t, fixtures, c.fixture)
			run := func(v any) (any, error) {
				stub := sendapitest.NewStub(t, sendapitest.Serve(statusOf(c.fixture), edit(t, base, func(m map[string]any) { c.inject(m, v) })))
				return c.call(t, stub.URL())
			}
			// the control: the recording itself is accepted (the key is absent or, for the ones it has, a different value)
			stub := sendapitest.NewStub(t, sendapitest.Serve(statusOf(c.fixture), base))
			if _, err := c.call(t, stub.URL()); err != nil {
				t.Fatalf("the recording itself is refused: %v", err)
			}
			v, err := run(c.good)
			if err != nil {
				t.Fatalf("a good value is refused: %v", err)
			}
			c.check(t, v)
			v, err = run(c.bad)
			if ce := wantKind(t, err, modclient.Contract); ce.Key != c.badKey || !isZero(v) {
				t.Fatalf("a bad value: key %q, want %q: %v", ce.Key, c.badKey, err)
			}
		})
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

// statusOf is the status a recorded success was served with.
func statusOf(fixture string) int {
	if strings.HasPrefix(fixture, "accounts.create.created") {
		return 201
	}
	return 200
}

// recordedFixtures are the 24 fixtures that were recorded from the real module 3 (record run 44211654839, 2026-11-18) and normalised by contractcheck.Normalise; the other 21 files of
// testdata/contract are still hand-written. testdata/README.md names both lists and TestReadmeNamesWhichFixturesAreRecorded holds it to this one.
var recordedFixtures = []string{
	"quote.ok.json", "quote.infeasible.json", "run.created.json", "run.replay.json", "run.error.not_found.json", "run.error.conflict.json",
	"report.job.sent.json", "report.job.error.not_found.json", "report.campaign.page.json", "report.campaign.empty.json",
	"accounts.list.page.json", "accounts.list.empty.json", "accounts.get.ok.json", "accounts.get.error.not_found.json",
	"accounts.create.created.json", "accounts.create.created_failed.json", "accounts.create.error.duplicate.json",
	"accounts.test.ok.json", "accounts.test.failed.json", "accounts.test.error.conflict.json", "accounts.test.error.not_found.json",
	"accounts.bulk.ok.json", "accounts.bulk.error.conflict.json", "accounts.events.empty.json",
}

// readmeSections splits testdata/README.md into the file names it lists under "## Recorded" and under "## Hand-written" (one backticked name per list item).
func readmeSections(text string) (recorded, hand []string) {
	var cur *[]string
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r", ""), "\n") {
		switch {
		case strings.HasPrefix(line, "## Recorded"):
			cur = &recorded
		case strings.HasPrefix(line, "## Hand-written"):
			cur = &hand
		case strings.HasPrefix(line, "## "):
			cur = nil
		case cur != nil && strings.HasPrefix(line, "- `"):
			if end := strings.Index(line[3:], "`"); end > 0 && 3+end+1 == len(strings.TrimRight(line, " ")) { // a bare list item only: an explanatory bullet that starts with a name is not a list entry
				*cur = append(*cur, line[3:3+end])
			}
		}
	}
	slices.Sort(recorded)
	slices.Sort(hand)
	return recorded, hand
}

// The README says which fixtures are recorded and which are still hand-written, and the two lists are the 24 recorded files and the other 21 (every file of the directory once, none in both),
// and it cites the record run. The control: a README that lists a recorded file as hand-written, or forgets one, is caught by the same comparison.
func TestReadmeNamesWhichFixturesAreRecorded(t *testing.T) {
	text := string(sendapitest.Read(t, "testdata", "README.md"))
	all := map[string]bool{}
	for _, c := range m3.Contracts() {
		all[c.Fixture] = true
	}
	var wantHand []string
	for f := range all {
		if !slices.Contains(recordedFixtures, f) {
			wantHand = append(wantHand, f)
		}
	}
	slices.Sort(wantHand)
	wantRec := slices.Clone(recordedFixtures)
	slices.Sort(wantRec)
	problems := func(text string) []string {
		var p []string
		rec, hand := readmeSections(text)
		if !slices.Equal(rec, wantRec) {
			p = append(p, fmt.Sprintf("the recorded list is %v, want %v", rec, wantRec))
		}
		if !slices.Equal(hand, wantHand) {
			p = append(p, fmt.Sprintf("the hand-written list is %v, want %v", hand, wantHand))
		}
		if !strings.Contains(text, "44211654839") || !strings.Contains(text, "2026-11-18") {
			p = append(p, "the record run and its date are not cited")
		}
		return p
	}
	if len(wantRec) != 24 || len(wantHand) != 21 {
		t.Fatalf("%d recorded and %d hand-written: the subject changed", len(wantRec), len(wantHand))
	}
	for _, f := range recordedFixtures {
		if !all[f] {
			t.Fatalf("%s is listed as recorded and no contract names it", f)
		}
	}
	if p := problems(text); len(p) != 0 {
		t.Fatalf("the README is out of step: %v", p)
	}
	moved := strings.Replace(text, "- `quote.ok.json`", "- `quote.gone.json`", 1)
	if p := problems(moved); len(p) == 0 {
		t.Fatal("a README that does not list a recorded file was not caught")
	}
	if p := problems(strings.ReplaceAll(text, "44211654839", "1")); len(p) != 1 || !strings.Contains(p[0], "record run") {
		t.Fatalf("a README that does not cite the run: %v", p)
	}
}

// testMatches holds a TestResult to the test button's answer it was decoded from.
func testMatches(t testing.TB, r m3.TestResult, d any) {
	t.Helper()
	same(t, "verification.state", string(r.Verification.State), mapped(asStr(t, d, "verification.state"), vocabSet(t, "test_verification_state")))
	code := ""
	if c := optStr(d, "verification.code"); c != nil {
		code = mapped(*c, vocabSet(t, "verify_code"))
	}
	same(t, "verification.code", string(r.Verification.Code), code)
	samePtr(t, "verification.smtp_response", r.Verification.SMTPResponse, optStr(d, "verification.smtp_response"))
	same(t, "from_alignment.state", string(r.Alignment.State), mapped(asStr(t, d, "from_alignment.state"), vocabSet(t, "test_alignment_state")))
	samePtr(t, "from_alignment.message", r.Alignment.Message, optStr(d, "from_alignment.message"))
	e := optStr(d, "from_alignment.error")
	same(t, "not stored", r.Alignment.NotStored, e != nil && *e != "")
}
