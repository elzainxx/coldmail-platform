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

	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi/sendapitest"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// Ten of the fixtures under testdata/contract are RECORDINGS of the real module 2 (drift session, run 44211663760, 2026-11-18), normalised by the harness: uuids become
// 00000000-0000-4000-8000-0000000000NN by first appearance, instants 2026-01-01T00:00:00Z, provider ids a run of A. The tests therefore never hard-code a value out of a recording
// (an id, a count, a state): they READ it from the fixture, through encoding/json, which is a different path from the adapter's decoding, and compare what the adapter returned
// with what the file says. The fixtures that were not reached stay hand-written and are read the same way.
var recordedFixtures = []string{
	"quote.ok.json", "run.created.json", "run.replay.json", "run.error.not_found.json", "report.job.sent.json", "report.job.error.not_found.json",
	"report.campaign.page.json", "report.campaign.last_page.json", "report.campaign.empty.json", "cancel.ok.json",
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

// An unknown, mistyped or foreign campaign is answered with an empty 200, and the adapter says only that nothing is known. The empty page is derived from the recorded page by taking
// its content out (see emptyPageFrom), and the control is the recorded page itself, which is NOT nothing known.
func TestAnEmptyPageIsNothingKnown(t *testing.T) {
	id := atStr(t, "report.campaign.page.json", "campaign_id")
	s := newStack(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		sendapitest.JSON(w, 200, string(emptyPageFrom(t, "report.campaign.page.json")))
	}, modclient.Options{})
	rep, err := s.client(t, userA).ReportCampaign(ctxT(t), id)
	if err != nil || !rep.NothingKnown() || !rep.ItemsMatchTotal() || rep.Progress.IsComplete || rep.Pages != 1 {
		t.Fatalf("%v %+v", err, rep)
	}
	recorded := newStack(t, reply(200, string(sendapitest.Read(t, fixtures, "report.campaign.page.json"))), modclient.Options{})
	rep, err = recorded.client(t, userA).ReportCampaign(ctxT(t), id)
	if err != nil || rep.NothingKnown() || !rep.ItemsMatchTotal() || int64(len(rep.Items)) != atInt(t, "report.campaign.page.json", "totals", "total") {
		t.Fatalf("the recorded page: %v %+v", err, rep.Totals)
	}
}

// The recorded fixtures carry only what the normalisation leaves: ids of the form 00000000-0000-4000-8000-0000000000NN, instants 2026-01-01T00:00:00Z, provider ids a run of A and request
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

// testdata/README.md names the recordings and the hand-written fixtures exactly: every recorded file is named under RECORDED, every other file of contract/ under HAND-WRITTEN, the run
// that recorded them is cited, and nothing is in both or in neither. The control runs the same comparison on a README that omits one file and on one that names a file twice.
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
	problems := func(doc string) []string {
		recSection, handSection, ok := strings.Cut(doc, "HAND-WRITTEN")
		if !ok || !strings.Contains(recSection, "RECORDED") {
			return []string{"the README has no RECORDED and HAND-WRITTEN sections"}
		}
		var out []string
		if !strings.Contains(doc, "44211663760") || !strings.Contains(doc, "2026-11-18") {
			out = append(out, "the README does not cite the recording run and date")
		}
		isRecorded := map[string]bool{}
		for _, n := range recordedFixtures {
			isRecorded[n] = true
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
	if p := problems(string(readme)); len(p) != 0 {
		t.Fatalf("the README and the fixtures disagree: %v", p)
	}
	if len(files) != len(recordedFixtures)+10 {
		t.Fatalf("%d fixtures, %d recorded: the README's hand-written list must be revisited when this changes", len(files), len(recordedFixtures))
	}
	omit := strings.Replace(string(readme), "`quote.ok.json`", "quote", 1)
	if p := problems(omit); len(p) == 0 {
		t.Fatal("a README that omits a recorded file passed")
	}
	twice := strings.Replace(string(readme), "HAND-WRITTEN", "HAND-WRITTEN `cancel.ok.json`", 1)
	if p := problems(twice); len(p) == 0 {
		t.Fatal("a README that names a recording as hand-written passed")
	}
	if p := problems("nothing"); len(p) == 0 {
		t.Fatal("an empty README passed")
	}
}
