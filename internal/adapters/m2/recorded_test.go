package m2_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/adapters/m2"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi/sendapitest"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// Every fixture under testdata/contract is a RECORDING of the real module 2 (the contract drift sessions of 2026-11-18: full record run 44225747596 and scoped record run
// 44225754836, and the Phase 3 close-out re-record, run 44395876225), normalised by the harness: uuids become 00000000-0000-4000-8000-0000000000NN by first appearance, instants 2026-01-01T00:00:00Z, provider ids a run of A. The tests
// therefore never hard-code a value out of a recording (an id, a count, a state, a cursor): they READ it from the fixture, through encoding/json, which is a different path from the
// adapter's decoding, and compare what the adapter returned with what the file says. They do not assume what a file name says about its content either (what a recording of a
// "page" holds is read from the page), and they do not assume that one recorded page is a whole campaign: see campaignPages.
var recordedFixtures = []string{
	"cancel.error.validation_failed.json", "cancel.ok.json", "describe.ok.json", "quote.ok.json",
	"report.campaign.empty.json", "report.campaign.error.cursor_forged.json", "report.campaign.error.cursor_malformed.json", "report.campaign.last_page.json", "report.campaign.page.json",
	"report.job.error.not_found.json", "report.job.queued.json", "report.job.sent.json",
	"run.created.json", "run.error.conflict.json", "run.error.internal_error.json", "run.error.not_found.json", "run.error.paused.json", "run.error.unauthenticated.json",
	"run.error.validation_failed.json", "run.replay.json",
}

// The normalisation's own spellings, named: what a recording may carry in place of an id, an instant and a provider id.
var (
	normalisedUUID      = regexp.MustCompile(`^00000000-0000-4000-8000-0000000000[0-9]{2}$`)
	normalisedInstant   = regexp.MustCompile(`^2026-01-01T00:00:00(\.0+)?Z$`)
	normalisedProvider  = regexp.MustCompile(`^A+$`)
	normalisedRequestID = regexp.MustCompile(`^a+$`)
)

// rec decodes a fixture into a generic document with encoding/json.
func rec(t testing.TB, name string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(sendapitest.Read(t, fixtures, name), &m); err != nil {
		t.Fatalf("%s is not JSON: %v", name, err)
	}
	return m
}

// at walks a decoded document: string elements are object keys.
func at(t testing.TB, v any, path ...string) any {
	t.Helper()
	for _, k := range path {
		m, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("the path %v leaves an object at %q", path, k)
		}
		v, ok = m[k]
		if !ok {
			t.Fatalf("the fixture has no key %v", path)
		}
	}
	return v
}

func atStr(t testing.TB, name string, path ...string) string {
	t.Helper()
	s, ok := at(t, rec(t, name), path...).(string)
	if !ok {
		t.Fatalf("%s %v is not a string", name, path)
	}
	return s
}

func atInt(t testing.TB, name string, path ...string) int64 {
	t.Helper()
	f, ok := at(t, rec(t, name), path...).(float64)
	if !ok {
		t.Fatalf("%s %v is not a number", name, path)
	}
	return int64(f)
}

// optStr is a string or null of a decoded item.
func optStr(v any) *string {
	if s, ok := v.(string); ok {
		return &s
	}
	return nil
}

func eqStr(a, b *string) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

// classOf is what each state means for settlement, written out here independently of the adapter's switch.
func classOf(state string) sendapi.Class {
	switch state {
	case "queued":
		return sendapi.ClassPending
	case "assigned", "sending":
		return sendapi.ClassInFlight
	}
	return sendapi.ClassTerminal
}

// checkItem compares an item the adapter returned with the item object of the fixture it was decoded from.
func checkItem(t testing.TB, got sendapi.Item, want map[string]any, where string) {
	t.Helper()
	state, _ := want["state"].(string)
	if got.State != state || got.Class != classOf(state) {
		t.Fatalf("%s: state %q (class %s), the fixture says %q", where, got.State, got.Class, state)
	}
	if id, ok := want["job_item_id"].(string); ok && got.JobItemID != id {
		t.Fatalf("%s: job item id %q, the fixture says %q", where, got.JobItemID, id)
	}
	if !eqStr(got.Reason, optStr(want["reason"])) || !eqStr(got.ErrorClass, optStr(want["error_class"])) || !eqStr(got.ProviderMessageID, optStr(want["provider_message_id"])) {
		t.Fatalf("%s: reason, error class or provider id differ from the fixture: %+v", where, got)
	}
	if n, ok := want["attempt_count"].(float64); !ok || got.AttemptCount != int64(n) {
		t.Fatalf("%s: attempt count %d, the fixture says %v", where, got.AttemptCount, want["attempt_count"])
	}
	switch c := want["completed_at"].(type) {
	case string:
		ts, err := time.Parse(time.RFC3339Nano, c)
		if err != nil || got.CompletedAt == nil || !got.CompletedAt.Equal(ts) {
			t.Fatalf("%s: completed_at %v, the fixture says %q", where, got.CompletedAt, c)
		}
	case nil:
		if got.CompletedAt != nil {
			t.Fatalf("%s: completed_at %v, the fixture says null or none", where, got.CompletedAt)
		}
	}
}

// checkPage compares a campaign page the adapter returned with the fixture it was decoded from: the campaign, the totals, the progress, the cursor, the page size and every item.
func checkPage(t testing.TB, got sendapi.CampaignPage, name string) {
	t.Helper()
	doc := rec(t, name)
	num := func(path ...string) int64 { f, _ := at(t, doc, path...).(float64); return int64(f) }
	if got.CampaignID != at(t, doc, "campaign_id") {
		t.Fatalf("%s: campaign %q", name, got.CampaignID)
	}
	tot := got.Totals
	if tot.Total != num("totals", "total") || tot.Sent != num("totals", "sent") || tot.Bounced != num("totals", "bounced") || tot.Failed != num("totals", "failed") ||
		tot.InFlight != num("totals", "in_flight") || tot.Pending != num("totals", "pending") || tot.WaitingForCapacity != num("totals", "waiting_for_capacity") || tot.Cancelled != num("totals", "cancelled") {
		t.Fatalf("%s: totals %+v differ from the fixture", name, tot)
	}
	if got.Progress.Terminal != num("progress", "terminal") || got.Progress.IsComplete != at(t, doc, "progress", "is_complete") {
		t.Fatalf("%s: progress %+v differs from the fixture", name, got.Progress)
	}
	if cur := optStr(at(t, doc, "items", "next_cursor")); (cur == nil) != (got.NextCursor == "") || (cur != nil && *cur != got.NextCursor) {
		t.Fatalf("%s: cursor %q differs from the fixture", name, got.NextCursor)
	}
	if int64(got.PageSize) != num("items", "page_size") || got.DispatchPaused != at(t, doc, "dispatch_paused") {
		t.Fatalf("%s: page size or dispatch_paused differ from the fixture", name)
	}
	results := at(t, doc, "items", "results").([]any)
	if len(got.Items) != len(results) || len(results) == 0 {
		t.Fatalf("%s: %d item(s), the fixture has %d", name, len(got.Items), len(results))
	}
	for i, r := range results {
		checkItem(t, got.Items[i], r.(map[string]any), fmt.Sprintf("%s item %d", name, i))
	}
}

// emptyPageFrom derives, from a recorded campaign page, the page a module answers for a campaign it knows nothing about: no items and all the counts zero. It is NOT a recording (the
// real run did not ask for an unknown campaign): it is the recorded page with its content taken out, so every key and type is the real module's.
func emptyPageFrom(t testing.TB, name string) []byte {
	t.Helper()
	body := sendapitest.Read(t, fixtures, name)
	for _, m := range []struct {
		path string
		v    any
	}{
		{"items.results", []any{}}, {"items.next_cursor", nil}, {"progress.terminal", 0}, {"progress.fraction_complete", 0}, {"progress.is_complete", false},
		{"totals.total", 0}, {"totals.sent", 0}, {"totals.bounced", 0}, {"totals.failed", 0}, {"totals.in_flight", 0}, {"totals.pending", 0},
		{"totals.waiting_for_capacity", 0}, {"totals.cancelled", 0},
	} {
		var err error
		if body, err = sendapitest.Mutate(body, m.path, sendapitest.Set, m.v); err != nil {
			t.Fatal(err)
		}
	}
	return body
}

// cutPage returns the recorded campaign report `name` with its items cut to results[from:to] and its next cursor set to next (nil for none): every other key and type is the module's.
func cutPage(t testing.TB, name string, from, to int, next any) []byte {
	t.Helper()
	results := at(t, rec(t, name), "items", "results").([]any)
	body := sendapitest.Read(t, fixtures, name)
	var err error
	if body, err = sendapitest.Mutate(body, "items.results", sendapitest.Set, results[from:to]); err != nil {
		t.Fatal(err)
	}
	if body, err = sendapitest.Mutate(body, "items.next_cursor", sendapitest.Set, next); err != nil {
		t.Fatal(err)
	}
	return body
}

// tailPageFrom derives the page that follows a recorded page that names a next cursor: the recorded page's last page is not among the recordings (a campaign is recorded as it was
// when the page was taken, and the page after it was not), so the tail is the recorded page with `total - items` items (at least one) copied from its first item under fresh ids, and no
// next cursor. With the recorded totals the walk then reads exactly the campaign the module said it had.
func tailPageFrom(t testing.TB, name string) []byte {
	t.Helper()
	doc := rec(t, name)
	results := at(t, doc, "items", "results").([]any)
	total, _ := at(t, doc, "totals", "total").(float64)
	n := int(total) - len(results)
	if n < 1 {
		n = 1
	}
	var tail []any
	for i := 0; i < n; i++ {
		item := map[string]any{}
		for k, v := range results[0].(map[string]any) {
			item[k] = v
		}
		item["job_item_id"] = fmt.Sprintf("00000000-0000-4000-8000-%012d", 9000+i)
		tail = append(tail, item)
	}
	body := sendapitest.Read(t, fixtures, name)
	var err error
	if body, err = sendapitest.Mutate(body, "items.results", sendapitest.Set, tail); err != nil {
		t.Fatal(err)
	}
	if body, err = sendapitest.Mutate(body, "items.next_cursor", sendapitest.Set, nil); err != nil {
		t.Fatal(err)
	}
	return body
}

// walkPages are the two pages of a campaign walk and the cursor between them. The first page is the recorded campaign report that names a next cursor (a real middle page) when there is
// one, with the tail page derived from it (tailPageFrom); when no recording names a cursor, it is the recorded page cut in half with a cursor that is not a recording. `recorded` says
// which. Nothing in the tests assumes that a recorded page is a whole campaign: a stub that serves one page again for the request that carries its cursor is refused by the walk
// (the same item on two pages), and the tests that walk a campaign serve these pages by the cursor value.
type walkPages struct {
	source      string
	first, tail []byte
	cursor      string
	recorded    bool
}

// pagesOf is the walk of one recorded campaign report: a report that names a next cursor is its first page, with the derived tail; one that does not is a whole campaign, served as it is.
func pagesOf(t testing.TB, name string) walkPages {
	t.Helper()
	w := walkPages{source: name, first: sendapitest.Read(t, fixtures, name), recorded: true}
	if cur := optStr(at(t, rec(t, name), "items", "next_cursor")); cur != nil {
		w.tail, w.cursor = tailPageFrom(t, name), *cur
	}
	return w
}

func campaignPages(t testing.TB) walkPages {
	t.Helper()
	for _, name := range []string{"report.campaign.page.json", "report.campaign.last_page.json", "report.campaign.empty.json"} {
		if optStr(at(t, rec(t, name), "items", "next_cursor")) != nil {
			return pagesOf(t, name)
		}
	}
	const name, cursor = "report.campaign.page.json", "AAAA" // opaque to the adapter, passed back verbatim
	n := len(at(t, rec(t, name), "items", "results").([]any))
	if n < 2 {
		t.Fatalf("%s has %d item(s): nothing to cut in two", name, n)
	}
	return walkPages{source: name, first: cutPage(t, name, 0, n/2, cursor), tail: cutPage(t, name, n/2, n, nil), cursor: cursor}
}

// follow is a stub that FOLLOWS THE CURSOR VALUE: the first page for a request with no cursor, the tail for the request that carries exactly the first page's cursor, a module fault for any
// other cursor (the stub cannot answer a cursor it did not issue).
func (w walkPages) follow() handler {
	first, tail := string(w.first), string(w.tail)
	return func(rw http.ResponseWriter, r *http.Request, _ int) {
		switch c := r.URL.Query().Get("cursor"); c {
		case "":
			sendapitest.JSON(rw, 200, first)
		case w.cursor:
			sendapitest.JSON(rw, 200, tail)
		default:
			sendapitest.JSON(rw, 500, sendapitest.Envelope("internal_error"))
		}
	}
}

// idsOf are the job item ids of a page body, in order.
func idsOf(t testing.TB, body []byte) []string {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range at(t, doc, "items", "results").([]any) {
		out = append(out, r.(map[string]any)["job_item_id"].(string))
	}
	return out
}

// A campaign report that names a next cursor is walked through its cursor: the walk reads the first page, then the tail for the request that carries the first page's cursor, every item
// once, in order, and the report's own total agrees with the number of items read. The paired control is the stub that does NOT follow the cursor (it serves the first page again): the
// walk refuses it as the same item on two pages, naming the key, and returns no report: a recorded page is not a whole campaign unless its cursor is null.
func TestARecordedMiddlePageIsWalkedThroughItsCursor(t *testing.T) {
	w := campaignPages(t)
	id := atStr(t, w.source, "campaign_id")
	s := newStack(t, w.follow(), modclient.Options{})
	rep, err := s.client(t, userA).ReportCampaign(ctxT(t), id)
	if err != nil {
		t.Fatal(err)
	}
	want := append(idsOf(t, w.first), idsOf(t, w.tail)...)
	var got []string
	for _, it := range rep.Items {
		got = append(got, it.JobItemID)
	}
	if problems := sendapitest.ExactSet(got, want); len(problems) != 0 || rep.Pages != 2 {
		t.Fatalf("pages %d, the walk is not the exact set: %v", rep.Pages, problems)
	}
	if reqs := s.stub.Requests(); len(reqs) != 2 || reqs[0].Query.Has("cursor") || reqs[1].Query.Get("cursor") != w.cursor {
		t.Fatalf("requests: %+v", reqs)
	}
	if w.recorded && !rep.ItemsMatchTotal() {
		t.Fatalf("the recorded middle page and its derived tail read %d item(s), the module's total says %d", len(rep.Items), rep.Totals.Total)
	}
	// control: the same first page served for every request (the old stub) is refused, and nothing is returned
	again := newStack(t, reply(200, string(w.first)), modclient.Options{})
	none, err := again.client(t, userA).ReportCampaign(ctxT(t), id)
	if ce := wantKind(t, err, modclient.Contract); ce.Key != "items.results" || !strings.Contains(ce.Message, "same job item") {
		t.Fatalf("key %q: %v", ce.Key, err)
	}
	if len(none.Items) != 0 || none.Pages != 0 {
		t.Fatalf("a report came back with the error: %+v", none)
	}
}

// An unknown, mistyped or foreign campaign is answered with an empty 200, and the adapter says only that nothing is known. Two subjects, each read for what it holds: the page
// derived from a recorded page by taking its content out (see emptyPageFrom), which is nothing known by construction, and the recording named `empty`, which is nothing known exactly
// when its file has no items and a total of 0. The control is a recorded page with items, which is NOT nothing known.
func TestAnEmptyPageIsNothingKnown(t *testing.T) {
	const src = "report.campaign.page.json"
	id := atStr(t, src, "campaign_id")
	derived := newStack(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		sendapitest.JSON(w, 200, string(emptyPageFrom(t, src)))
	}, modclient.Options{})
	rep, err := derived.client(t, userA).ReportCampaign(ctxT(t), id)
	if err != nil || !rep.NothingKnown() || !rep.ItemsMatchTotal() || rep.Progress.IsComplete || rep.Pages != 1 {
		t.Fatalf("%v %+v", err, rep)
	}
	const named = "report.campaign.empty.json"
	n := int64(len(at(t, rec(t, named), "items", "results").([]any)))
	total := atInt(t, named, "totals", "total")
	namedStack := newStack(t, pagesOf(t, named).follow(), modclient.Options{})
	rep, err = namedStack.client(t, userA).ReportCampaign(ctxT(t), atStr(t, named, "campaign_id"))
	if err != nil || rep.NothingKnown() != (n == 0 && total == 0) || rep.Totals.Total != total || rep.ItemsMatchTotal() != (int64(len(rep.Items)) == total) {
		t.Fatalf("the recording named empty (%d item(s), total %d): %v %+v", n, total, err, rep.Totals)
	}
	// control: a recorded page with items is not nothing known
	if n := int64(len(at(t, rec(t, src), "items", "results").([]any))); n == 0 {
		t.Fatal("the control page has no items, so the control could not fail")
	}
	recorded := newStack(t, pagesOf(t, src).follow(), modclient.Options{})
	rep, err = recorded.client(t, userA).ReportCampaign(ctxT(t), id)
	if err != nil || rep.NothingKnown() || len(rep.Items) == 0 {
		t.Fatalf("the recorded page: %v %+v", err, rep.Totals)
	}
}

// The recorded fixtures (all of them) carry only what the normalisation leaves: ids of the form 00000000-0000-4000-8000-0000000000NN, instants 2026-01-01T00:00:00Z, provider ids a run of A and request
// ids a run of a; no host name, no address, no token. A recording that kept a real id or instant would fail here, and the control runs the same scan on a body that does.
func TestRecordedFixturesAreNormalised(t *testing.T) {
	scan := func(body []byte) []string {
		var doc any
		if err := json.Unmarshal(body, &doc); err != nil {
			return []string{"not JSON: " + err.Error()}
		}
		var problems []string
		var walk func(path string, v any)
		walk = func(path string, v any) {
			switch x := v.(type) {
			case map[string]any:
				for k, e := range x {
					walk(path+"."+k, e)
				}
			case []any:
				for i, e := range x {
					walk(path+"[]", e)
					_ = i
				}
			case string:
				leaf := path[strings.LastIndex(path, ".")+1:]
				switch {
				case strings.HasSuffix(leaf, "_id") && leaf != "request_id" && leaf != "provider_message_id" && !normalisedUUID.MatchString(x):
					problems = append(problems, path+" is not a normalised uuid")
				case strings.HasSuffix(leaf, "_at") && !normalisedInstant.MatchString(x):
					problems = append(problems, path+" is not the normalised instant")
				case leaf == "provider_message_id" && !normalisedProvider.MatchString(x):
					problems = append(problems, path+" is not a normalised provider id")
				case leaf == "request_id" && !normalisedRequestID.MatchString(x):
					problems = append(problems, path+" is not a normalised request id")
				case strings.ContainsAny(x, "@\\"):
					problems = append(problems, path+" carries an address or a path")
				}
			}
		}
		walk("", doc)
		return problems
	}
	for _, name := range recordedFixtures {
		if p := scan(sendapitest.Read(t, fixtures, name)); len(p) != 0 {
			t.Errorf("%s is not normalised: %v", name, p)
		}
	}
	// control: the scan catches each way a recording could keep something real
	for name, body := range map[string]string{
		"a real uuid":       `{"campaign_id":"5a1c9e30-7d2b-4f6a-9b8e-1c3d5e7f9a02"}`,
		"a real instant":    `{"completed_at":"2026-11-17T10:00:00Z"}`,
		"a real provider":   `{"provider_message_id":"<job-1@mail.example.test>"}`,
		"a real request id": `{"error":{"request_id":"req_0123456789abcdef0123456789abcdef"}}`,
		"an address":        `{"x":"someone@example.test"}`,
	} {
		if len(scan([]byte(body))) == 0 {
			t.Errorf("the scan passed %s", name)
		}
	}
}

// testdata/README.md names the recordings and the hand-written fixtures exactly: every recorded file is named under RECORDED, every other file of contract/ (there is none now: every
// contract was reached) under HAND-WRITTEN, both recording runs are cited with their date, and nothing is in both or in neither. The control runs the same comparison on a README that
// omits one file, one that names a recording as hand-written, one that omits a run, and an empty one.
func TestReadmeNamesTheRecordedAndHandWrittenFixtures(t *testing.T) {
	readme, err := os.ReadFile(filepath.Join("testdata", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(fixtures)
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, e := range entries {
		files = append(files, e.Name())
	}
	isRecorded := map[string]bool{}
	for _, n := range recordedFixtures {
		isRecorded[n] = true
	}
	problems := func(doc string, files []string) []string {
		recSection, handSection, ok := strings.Cut(doc, "HAND-WRITTEN")
		if !ok || !strings.Contains(recSection, "RECORDED") {
			return []string{"the README has no RECORDED and HAND-WRITTEN sections"}
		}
		var out []string
		for _, run := range []string{"44225747596", "44225754836", "44395876225", "2026-11-18"} {
			if !strings.Contains(doc, run) {
				out = append(out, "the README does not cite "+run)
			}
		}
		for _, f := range files {
			inRec, inHand := strings.Contains(recSection, "`"+f+"`"), strings.Contains(handSection, "`"+f+"`")
			switch {
			case isRecorded[f] && (!inRec || inHand):
				out = append(out, f+" is recorded and the README does not say so exactly once")
			case !isRecorded[f] && (inRec || !inHand):
				out = append(out, f+" is hand-written and the README does not say so exactly once")
			}
		}
		return out
	}
	if p := problems(string(readme), files); len(p) != 0 {
		t.Fatalf("the README and the fixtures disagree: %v", p)
	}
	onDisk := map[string]bool{}
	for _, f := range files {
		onDisk[f] = true
	}
	for _, n := range recordedFixtures {
		if !onDisk[n] {
			t.Fatalf("%s is listed as recorded and is not in the directory", n)
		}
	}
	if len(files) != len(m2.Contracts()) {
		t.Fatalf("%d fixtures for %d contracts", len(files), len(m2.Contracts()))
	}
	omit := strings.Replace(string(readme), "`quote.ok.json`", "quote", 1)
	if p := problems(omit, files); len(p) == 0 {
		t.Fatal("a README that omits a recorded file passed")
	}
	twice := strings.Replace(string(readme), "HAND-WRITTEN", "HAND-WRITTEN `cancel.ok.json`", 1)
	if p := problems(twice, files); len(p) == 0 {
		t.Fatal("a README that names a recording as hand-written passed")
	}
	oneRun := strings.ReplaceAll(string(readme), "44225754836", "0")
	if p := problems(oneRun, files); len(p) == 0 {
		t.Fatal("a README that omits a recording run passed")
	}
	noCloseout := strings.ReplaceAll(string(readme), "44395876225", "0")
	if p := problems(noCloseout, files); len(p) == 0 {
		t.Fatal("a README that omits the close-out recording run passed")
	}
	if p := problems("nothing", files); len(p) == 0 {
		t.Fatal("an empty README passed")
	}
	// the control the other way: a file in the directory that is neither recorded nor named as hand-written is caught
	if p := problems(string(readme), append(append([]string(nil), files...), "stray.json")); len(p) != 1 || !strings.Contains(p[0], "stray.json is hand-written") {
		t.Fatalf("a stray file was not caught: %v", p)
	}
}
