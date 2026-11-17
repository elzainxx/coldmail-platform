package m1results

import (
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/charge"
	"github.com/coldmail-platform/platform/internal/money"
)

// The bodies below are copied verbatim from module 1's own contract, Docs/bulk-item-identity.md, section 5 (a result element) and the 202 / status
// examples of sections 3 and 4. The envelope around the elements ({"job_id","results","next_cursor"}) is the one api.go's handleJobResults writes.
// Nothing here is invented beyond that: a decoder tested on bodies written by the person who wrote the decoder proves nothing about the module.

// docResultElement is section 5's example: row 2 of the job, a repeat of row 0.
const docResultElement = `{
  "api_version": "v1", "status": "valid", "reason": "accepted", "confidence": 90, "recommendation": "send",
  "email": "", "domain": "acmecorp.com", "flags": {"from_cache": false}, "signals": {"smtp_code": "250"},
  "meta": {"request_id": "", "checked_at": "", "latency_ms": 0, "policy": "cold-email", "policy_version": 1,
           "verifier_ip": null, "node_id": null, "engine_version": null, "config_fingerprint": null, "from_cache": false},
  "raw": null,
  "item_id": "row-2",
  "ordinal": 2,
  "duplicate_of": 0
}`

// docMalformedElement is what section 5 says a malformed row's element is: invalid / invalid_syntax / skip, confidence 99, signals {"syntax_valid": false},
// no domain, in the same verdict object (its item_id and ordinal are the row's).
const docMalformedElement = `{
  "api_version": "v1", "status": "invalid", "reason": "invalid_syntax", "confidence": 99, "recommendation": "skip",
  "email": "", "domain": "", "flags": {"from_cache": false}, "signals": {"syntax_valid": false},
  "meta": {"request_id": "", "checked_at": "", "latency_ms": 0, "policy": "cold-email", "policy_version": 1,
           "verifier_ip": null, "node_id": null, "engine_version": null, "config_fingerprint": null, "from_cache": false},
  "raw": null,
  "item_id": "row-3",
  "ordinal": 3
}`

// element builds one result element for a test that needs a row the module's doc does not show (an unknown with its origin, a cached answer). The keys
// are the verdict object's own: status, reason, flags.from_cache, signals.*; and the three identity keys.
func element(status, reason, itemID string, ordinal int, extra string) string {
	var b strings.Builder
	b.WriteString(`{"api_version":"v1","status":` + strconv.Quote(status) + `,"reason":` + strconv.Quote(reason))
	b.WriteString(`,"confidence":10,"recommendation":"review","email":"","domain":"","flags":{"from_cache":false},"signals":{},"raw":null`)
	if itemID != "" {
		b.WriteString(`,"item_id":` + strconv.Quote(itemID))
	}
	b.WriteString(`,"ordinal":` + strconv.Itoa(ordinal))
	if extra != "" {
		b.WriteString("," + extra)
	}
	b.WriteString("}")
	return b.String()
}

func decodeRow(t *testing.T, body string) (Row, error) {
	t.Helper()
	var r Row
	err := json.Unmarshal([]byte(body), &r)
	return r, err
}

func mustRow(t *testing.T, body string) Row {
	t.Helper()
	r, err := decodeRow(t, body)
	if err != nil {
		t.Fatalf("a result element that must decode was refused: %v\n%s", err, body)
	}
	return r
}

func ptr(n int) *int { return &n }

// TestRow_DecodesTheDocumentedElement: section 5's element decodes to exactly the fields the platform acts on, and every key it does not read (email, meta,
// raw, confidence, ...) is ignored. Paired control: the malformed element of the same section decodes to different values, so the decoder is not returning a
// constant, and the zero-valued duplicate_of of the first element is a pointer to 0, not an absent key.
func TestRow_DecodesTheDocumentedElement(t *testing.T) {
	got := mustRow(t, docResultElement)
	want := Row{Ordinal: 2, ItemID: "row-2", DuplicateOf: ptr(0), Status: "valid", Reason: "accepted"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("section 5's element decoded to %+v, want %+v", got, want)
	}
	if got.DuplicateOf == nil || *got.DuplicateOf != 0 {
		t.Fatal("duplicate_of: 0 is a real value (the first row of the job); it must decode to a pointer to 0, never to absent")
	}

	ctl := mustRow(t, docMalformedElement)
	wantCtl := Row{Ordinal: 3, ItemID: "row-3", Status: "invalid", Reason: "invalid_syntax"}
	if !reflect.DeepEqual(ctl, wantCtl) {
		t.Fatalf("control: the malformed element decoded to %+v, want %+v", ctl, wantCtl)
	}
	if ctl.DuplicateOf != nil {
		t.Fatal("control: a malformed row is never a duplicate, and carries no duplicate_of")
	}
	if reflect.DeepEqual(got, ctl) {
		t.Fatal("control: two different elements decoded to the same Row: the decoder returns something that does not depend on its input")
	}
}

// TestRow_ReadsOriginSkipTokenAndCache: the three provenance fields the charge table reads come out of their nested places (flags.from_cache,
// signals.smtp_skipped_reason, signals.failure_origin). Control: the same element without them decodes to empty values.
func TestRow_ReadsOriginSkipTokenAndCache(t *testing.T) {
	body := `{"api_version":"v1","status":"unknown","reason":"dns_error","confidence":10,"recommendation":"review","email":"","domain":"gone.example",` +
		`"flags":{"from_cache":true},"signals":{"smtp_skipped_reason":"smtp_unwired","failure_origin":"remote","error_capped":true,"retry_after_hint_s":30},` +
		`"item_id":"row-7","ordinal":7}`
	got := mustRow(t, body)
	want := Row{Ordinal: 7, ItemID: "row-7", Status: "unknown", Reason: "dns_error", FromCache: true, SkipReason: "smtp_unwired", FailureOrigin: "remote"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded %+v, want %+v", got, want)
	}

	ctl := mustRow(t, element("unknown", "dns_error", "row-7", 7, ""))
	if ctl.FromCache || ctl.SkipReason != "" || ctl.FailureOrigin != "" {
		t.Fatalf("control: an element with no provenance decoded to %+v: the three fields must read as empty when the module sends nothing", ctl)
	}
}

// TestRow_StrictOnStatusAndOrdinal: a result with no (or an empty, or a null) status, no (or a negative) ordinal, or a duplicate_of that is not an
// EARLIER row is refused, with ErrBadRow and a message that names what is wrong. Each case is paired with the control that the same element, repaired,
// decodes.
func TestRow_StrictOnStatusAndOrdinal(t *testing.T) {
	good := `{"status":"valid","reason":"accepted","item_id":"a","ordinal":4}`
	if _, err := decodeRow(t, good); err != nil {
		t.Fatalf("control: the repaired element is refused: %v", err)
	}
	cases := []struct{ name, body, mention string }{
		{"no status", `{"reason":"accepted","item_id":"a","ordinal":4}`, "no status"},
		{"an empty status", `{"status":"","reason":"accepted","item_id":"a","ordinal":4}`, "no status"},
		{"a blank status", `{"status":"  ","reason":"accepted","item_id":"a","ordinal":4}`, "no status"},
		{"a null status", `{"status":null,"reason":"accepted","item_id":"a","ordinal":4}`, "no status"},
		{"no ordinal", `{"status":"valid","reason":"accepted","item_id":"a"}`, "no ordinal"},
		{"a null ordinal", `{"status":"valid","reason":"accepted","item_id":"a","ordinal":null}`, "no ordinal"},
		{"a negative ordinal", `{"status":"valid","reason":"accepted","item_id":"a","ordinal":-1}`, "negative ordinal"},
		{"a duplicate of itself", `{"status":"valid","reason":"accepted","item_id":"a","ordinal":4,"duplicate_of":4}`, "not an earlier row"},
		{"a duplicate of a later row", `{"status":"valid","reason":"accepted","item_id":"a","ordinal":4,"duplicate_of":9}`, "not an earlier row"},
		{"a duplicate of a negative row", `{"status":"valid","reason":"accepted","item_id":"a","ordinal":4,"duplicate_of":-2}`, "not an earlier row"},
		{"a status of the wrong type", `{"status":7,"reason":"accepted","item_id":"a","ordinal":4}`, "documented types"},
		{"an ordinal of the wrong type", `{"status":"valid","reason":"accepted","item_id":"a","ordinal":"4"}`, "documented types"},
		{"an element that is not an object", `[1,2]`, "documented types"},
	}
	for _, c := range cases {
		_, err := decodeRow(t, c.body)
		if err == nil {
			t.Errorf("%s: the element was accepted: %s", c.name, c.body)
			continue
		}
		if !errors.Is(err, ErrBadRow) || !strings.Contains(err.Error(), c.mention) {
			t.Errorf("%s: refused with %q, want errors.Is(ErrBadRow) and a message containing %q", c.name, err, c.mention)
		}
	}
}

// TestRow_ErrorsCarryNoValueFromTheElement: a refused element's error text is built from fixed words and ordinals only. An element whose item_id, domain and
// reason hold an address (a module that echoes what it was given) puts none of it in the error.
func TestRow_ErrorsCarryNoValueFromTheElement(t *testing.T) {
	const addr = "jane.private@leak-check.example"
	bad := `{"status":"","reason":"` + addr + `","domain":"leak-check.example","item_id":"` + addr + `","ordinal":4,"email":"` + addr + `"}`
	_, err := decodeRow(t, bad)
	if err == nil {
		t.Fatal("an element with an empty status was accepted")
	}
	if leaks(err.Error(), addr, "leak-check.example") {
		t.Fatalf("the error text carries a value taken from the element: %q", err)
	}
	// the control of the instrument: the leak check does see an address that is there
	if !leaks("refused: "+addr, addr) {
		t.Fatal("control: leaks() did not see an address in a string that holds one: the check above proves nothing")
	}
}

// ---- ToSettleItems ----

// TestToSettleItems_ItemRefIsTheItemIDNeverTheOrdinal: the money.SettleItem's item_ref is the module's item_id. The labels here are deliberately not the
// ordinals, and the control shows that an ordinal-keyed item would differ from every one of them.
func TestToSettleItems_ItemRefIsTheItemIDNeverTheOrdinal(t *testing.T) {
	rows := []Row{
		{Ordinal: 0, ItemID: "acct-77/row-9", Status: "valid", Reason: "accepted"},
		{Ordinal: 1, ItemID: "7", Status: "invalid", Reason: "no_mx"},
		{Ordinal: 2, ItemID: "0", Status: "risky", Reason: "disposable"},
	}
	items, err := ToSettleItems(rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != len(rows) {
		t.Fatalf("%d item(s) for %d row(s)", len(items), len(rows))
	}
	for i, it := range items {
		if it.ItemRef != rows[i].ItemID {
			t.Errorf("row %d: item_ref %q, want the item_id %q", i, it.ItemRef, rows[i].ItemID)
		}
		if it.ItemRef == strconv.Itoa(rows[i].Ordinal) {
			t.Errorf("row %d: the item_ref %q is the ordinal: a position is not a key", i, it.ItemRef)
		}
		if it.Outcome.Module != "m1" || it.Outcome.State != rows[i].Status || it.Outcome.Reason != rows[i].Reason {
			t.Errorf("row %d: outcome %+v is not module 1's own words (%s/%s)", i, it.Outcome, rows[i].Status, rows[i].Reason)
		}
	}
	// control: rows whose item_id IS their ordinal's spelling would be indistinguishable from an ordinal key, which is why the rows above are not
	same, err := ToSettleItems([]Row{{Ordinal: 5, ItemID: "5", Status: "valid", Reason: "accepted"}})
	if err != nil || same[0].ItemRef != "5" {
		t.Fatalf("control: %v %+v", err, same)
	}
}

// TestToSettleItems_ADuplicateIsThePlatformDuplicateOutcome: a row with duplicate_of is platform/duplicate whatever verdict it carries (the first row of the
// address carries the charge), and it is the pointer's PRESENCE that decides, not its value: duplicate_of 0 is a duplicate of row 0. Control: the same
// verdict with no duplicate_of is module 1's own outcome.
func TestToSettleItems_ADuplicateIsThePlatformDuplicateOutcome(t *testing.T) {
	first := Row{Ordinal: 0, ItemID: "row-0", Status: "valid", Reason: "accepted"}
	repeat := Row{Ordinal: 2, ItemID: "row-2", DuplicateOf: ptr(0), Status: "valid", Reason: "accepted", FromCache: true, SkipReason: "x", FailureOrigin: "remote"}
	items, err := ToSettleItems([]Row{first, repeat})
	if err != nil {
		t.Fatal(err)
	}
	if want := (charge.Outcome{Module: "platform", State: "duplicate"}); items[1].Outcome != want {
		t.Fatalf("the repeat's outcome is %+v, want exactly %+v (no reason, no cache flag, no origin: it is the platform's own class)", items[1].Outcome, want)
	}
	if want := (charge.Outcome{Module: "m1", State: "valid", Reason: "accepted"}); items[0].Outcome != want {
		t.Fatalf("control: the first row's outcome is %+v, want module 1's own %+v", items[0].Outcome, want)
	}
	if items[1].ItemRef != "row-2" {
		t.Fatalf("the repeat keeps its own item_id as item_ref, got %q", items[1].ItemRef)
	}
}

// TestToSettleItems_AnUnknownRowCarriesItsOrigin: the failure origin, the SMTP skip token and the cache flag travel into the outcome unchanged, for every
// value, so that money.Settle can decide on them. Control: two rows that differ only in the origin give outcomes that differ only in the origin.
func TestToSettleItems_AnUnknownRowCarriesItsOrigin(t *testing.T) {
	for _, origin := range []string{"own", "remote", "undetermined", ""} {
		items, err := ToSettleItems([]Row{{Ordinal: 3, ItemID: "row-3", Status: "unknown", Reason: "dns_error", FailureOrigin: origin}})
		if err != nil {
			t.Fatal(err)
		}
		want := charge.Outcome{Module: "m1", State: "unknown", Reason: "dns_error", FailureOrigin: origin}
		if items[0].Outcome != want {
			t.Errorf("origin %q: outcome %+v, want %+v", origin, items[0].Outcome, want)
		}
	}
	items, err := ToSettleItems([]Row{{Ordinal: 1, ItemID: "x", Status: "risky", Reason: "plausible", FromCache: true, SkipReason: "smtp_unwired", FailureOrigin: "own"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := (charge.Outcome{Module: "m1", State: "risky", Reason: "plausible", FromCache: true, SkipReason: "smtp_unwired", FailureOrigin: "own"}); items[0].Outcome != want {
		t.Fatalf("outcome %+v, want %+v", items[0].Outcome, want)
	}
	a, _ := ToSettleItems([]Row{{ItemID: "x", Status: "unknown", Reason: "dns_error", FailureOrigin: "own"}})
	b, _ := ToSettleItems([]Row{{ItemID: "x", Status: "unknown", Reason: "dns_error", FailureOrigin: "remote"}})
	if a[0].Outcome == b[0].Outcome {
		t.Fatal("control: rows that differ only in the failure origin gave the same outcome: the origin is dropped on the way")
	}
}

// TestToSettleItems_NoItemIDIsAnError: a result with no item_id (absent, empty, blank) is ErrNoItemID, naming the ordinal; and a page that holds one such
// row is refused whole and returns nothing (a page that cannot be keyed completely is not settled in part). Control: the same page with the label present
// is accepted in full.
func TestToSettleItems_NoItemIDIsAnError(t *testing.T) {
	absent := mustRow(t, element("valid", "accepted", "", 4, "")) // no item_id key at all
	if absent.ItemID != "" {
		t.Fatalf("setup: an element with no item_id decoded to %q", absent.ItemID)
	}
	for _, id := range []string{"", " ", "\t"} {
		page := []Row{
			{Ordinal: 3, ItemID: "row-3", Status: "valid", Reason: "accepted"},
			{Ordinal: 4, ItemID: id, Status: "valid", Reason: "accepted"},
		}
		items, err := ToSettleItems(page)
		if !errors.Is(err, ErrNoItemID) {
			t.Fatalf("item_id %q: got %v, want ErrNoItemID", id, err)
		}
		if !strings.Contains(err.Error(), "ordinal 4") {
			t.Errorf("item_id %q: the error does not name the ordinal: %v", id, err)
		}
		if items != nil {
			t.Errorf("item_id %q: %d item(s) were returned beside the error: a page is settled whole or not at all", id, len(items))
		}
	}
	if _, err := ToSettleItems([]Row{absent}); !errors.Is(err, ErrNoItemID) {
		t.Fatalf("a decoded element without item_id: got %v, want ErrNoItemID", err)
	}
	ok := absent
	ok.ItemID = "row-4"
	if items, err := ToSettleItems([]Row{{Ordinal: 3, ItemID: "row-3", Status: "valid", Reason: "accepted"}, ok}); err != nil || len(items) != 2 {
		t.Fatalf("control: the same page with its labels present: %d item(s), %v", len(items), err)
	}
}

// TestToSettleItems_ProducesWhatMoneySettleAccepts: the items are the shape money.Settle takes (compile-time: money.SettleItem) and their item_refs are
// unique for rows with unique item_ids, which money's validation requires. Control: two rows with one item_id give two items with one item_ref, which is the
// thing money.Settle refuses (ErrBadItem): the platform's mapping does not hide it by renaming.
func TestToSettleItems_ProducesWhatMoneySettleAccepts(t *testing.T) {
	var _ []money.SettleItem = mustItems(t, []Row{{ItemID: "a", Status: "valid", Reason: "accepted"}})
	items := mustItems(t, []Row{{Ordinal: 0, ItemID: "a", Status: "valid", Reason: "accepted"}, {Ordinal: 1, ItemID: "b", Status: "valid", Reason: "accepted"}})
	if items[0].ItemRef == items[1].ItemRef {
		t.Fatal("two rows with two item_ids gave one item_ref")
	}
	clash := mustItems(t, []Row{{Ordinal: 0, ItemID: "a", Status: "valid", Reason: "accepted"}, {Ordinal: 1, ItemID: "a", Status: "valid", Reason: "accepted"}})
	if clash[0].ItemRef != clash[1].ItemRef {
		t.Fatal("control: two rows with one item_id were given different item_refs: the mapping invents keys")
	}
}

func mustItems(t *testing.T, rows []Row) []money.SettleItem {
	t.Helper()
	items, err := ToSettleItems(rows)
	if err != nil {
		t.Fatal(err)
	}
	return items
}
