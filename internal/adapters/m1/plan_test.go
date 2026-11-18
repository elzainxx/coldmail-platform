package m1

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/charge"
	"github.com/coldmail-platform/platform/internal/modclient"
	"github.com/coldmail-platform/platform/internal/norm"
)

// TestPrepare_AMalformedAddressIsNeverSentAndIsFree: of five inputs three fail norm/v1 (no '@', two '@', empty local part). Prepare is pure (nothing has contacted the stand-in when
// it returns), lists the three with their reasons in input order, sends only the two good rows, and the three malformed rows settle for 0 credits: charge.Decide answers Free by
// rule P-08. The request body that reaches module 1 holds neither malformed address. Paired control: SubmitRaw of the very same rows DOES send them, so the absence above is
// Prepare's doing and not the stand-in's.
func TestPrepare_AMalformedAddressIsNeverSentAndIsFree(t *testing.T) {
	inputs := []Input{
		{"r0", "ada@acme.example"},
		{"r1", "no-at-sign.example"},
		{"r2", "two@@signs.example"},
		{"r3", "bob@acme.example"},
		{"r4", "@no-local-part.example"},
	}
	f := serve(t, 202, `{"job_id":"`+testJob+`","status":"accepted","counts":{"total":2,"enqueued":2,"duplicate":0,"malformed":0},"malformed_rows":[],"malformed_rows_truncated":false,"processing_paused":false}`)
	plan := Prepare(inputs)
	if f.count() != 0 {
		t.Fatalf("Prepare made %d request(s): it must be pure", f.count())
	}
	if err := plan.Err(); err != nil {
		t.Fatal(err)
	}
	wantItems := []Item{{"r0", "ada@acme.example"}, {"r3", "bob@acme.example"}}
	if fmt.Sprint(plan.Items) != fmt.Sprint(wantItems) {
		t.Fatalf("items to send %v, want %v", plan.Items, wantItems)
	}
	wantBad := []Malformed{{"r1", norm.ReasonNoAt}, {"r2", norm.ReasonMultipleAt}, {"r4", norm.ReasonEmptyLocal}}
	if fmt.Sprint(plan.Malformed) != fmt.Sprint(wantBad) {
		t.Fatalf("malformed rows %v, want %v (in input order, with the norm/v1 reason)", plan.Malformed, wantBad)
	}
	if plan.Rows() != len(inputs) {
		t.Fatalf("the plan covers %d row(s) of %d", plan.Rows(), len(inputs))
	}
	settle := plan.MalformedSettleItems()
	if len(settle) != 3 {
		t.Fatalf("%d settle item(s) for 3 malformed rows", len(settle))
	}
	for i, it := range settle {
		if it.ItemRef != wantBad[i].ItemRef {
			t.Errorf("settle item %d is keyed %q, want %q", i, it.ItemRef, wantBad[i].ItemRef)
		}
		d := charge.Decide(it.Outcome)
		if d.Kind != charge.Free || d.Rule != "P-08" {
			t.Errorf("malformed row %s decides %+v, want Free by rule P-08", it.ItemRef, d)
		}
	}

	a := f.adapter(t)
	if _, _, err := a.Submit(testCtx(t), plan); err != nil {
		t.Fatal(err)
	}
	body := string(f.seen()[0].Body)
	for _, bad := range []string{"no-at-sign.example", "two@@signs.example", "no-local-part.example"} {
		if strings.Contains(body, bad) {
			t.Errorf("the request body carries the malformed address %q: %s", bad, body)
		}
	}
	if !strings.Contains(body, "ada@acme.example") || !strings.Contains(body, "bob@acme.example") {
		t.Errorf("the request body lacks the good rows: %s", body)
	}

	// control: the unfiltered path sends them
	g := serve(t, 202, `{"job_id":"`+testJob+`","status":"accepted","counts":{"total":5,"enqueued":2,"duplicate":0,"malformed":3},"malformed_rows":[],"malformed_rows_truncated":false,"processing_paused":false}`)
	raw := make([]Item, len(inputs))
	for i, in := range inputs {
		raw[i] = Item{ItemID: in.ItemRef, Address: in.Address}
	}
	id, counts, err := g.adapter(t).SubmitRaw(testCtx(t), raw)
	if err != nil || id != testJob || counts != (SubmitCounts{Total: 5, Enqueued: 2, Malformed: 3}) {
		t.Fatalf("control: SubmitRaw gave %q %+v %v", id, counts, err)
	}
	if rawBody := string(g.seen()[0].Body); !strings.Contains(rawBody, "no-at-sign.example") || !strings.Contains(rawBody, "two@@signs.example") {
		t.Fatalf("control: SubmitRaw did not send the malformed rows: %s", rawBody)
	}
}

// TestPrepare_DuplicatesAreAllSubmittedAndCounted: addresses that normalise to the same string (case, surrounding space) are ALL in Items, in order, and counted as duplicates;
// module 1 answers each and marks the repeats. Expected is total over rows, enqueued over distinct addresses. Control: four distinct addresses are no duplicates.
func TestPrepare_DuplicatesAreAllSubmittedAndCounted(t *testing.T) {
	plan := Prepare([]Input{{"a", "ada@acme.example"}, {"b", "ADA@Acme.Example"}, {"c", " ada@acme.example "}, {"d", "bob@acme.example"}})
	if len(plan.Items) != 4 || plan.Duplicates != 2 {
		t.Fatalf("%d item(s), %d duplicate(s), want 4 and 2", len(plan.Items), plan.Duplicates)
	}
	if want := (SubmitCounts{Total: 4, Enqueued: 2, Duplicate: 2}); plan.Expected() != want {
		t.Fatalf("expected counts %+v, want %+v", plan.Expected(), want)
	}
	ctl := Prepare([]Input{{"a", "ada@acme.example"}, {"b", "bob@acme.example"}, {"c", "cyd@acme.example"}, {"d", "dee@acme.example"}})
	if ctl.Duplicates != 0 || ctl.Expected() != (SubmitCounts{Total: 4, Enqueued: 4}) {
		t.Fatalf("control: four distinct addresses gave %d duplicate(s), expected %+v", ctl.Duplicates, ctl.Expected())
	}
	// a repeated MALFORMED address is not a duplicate (module 1: "A repeated malformed address is not a duplicate"): both are malformed and neither is sent
	m := Prepare([]Input{{"a", "nope"}, {"b", "nope"}})
	if len(m.Items) != 0 || len(m.Malformed) != 2 || m.Duplicates != 0 {
		t.Fatalf("two identical malformed rows: %d item(s), %d malformed, %d duplicate(s)", len(m.Items), len(m.Malformed), m.Duplicates)
	}
}

// TestPrepare_ARefusedBatchNamesTheInput: an item_ref module 1 (or the ledger) would not take, on ANY input (a malformed row needs one too), refuses the whole plan with an Invalid
// error that names the input and the field and never quotes the value; Submit returns that error before any request. Each case is paired with the repaired control.
func TestPrepare_ARefusedBatchNamesTheInput(t *testing.T) {
	good := func(ref string) []Input {
		return []Input{{"ok-0", "ada@acme.example"}, {ref, "no-at-sign"}, {"ok-2", "bob@acme.example"}}
	}
	cases := []struct{ name, ref, key string }{
		{"blank", "   ", "inputs[1].item_ref"},
		{"empty", "", "inputs[1].item_ref"},
		{"201 bytes", strings.Repeat("r", 201), "inputs[1].item_ref"},
		{"a newline", "r1\nx", "inputs[1].item_ref"},
		{"a NUL", "r1\x00", "inputs[1].item_ref"},
		{"a DEL", "r1\x7f", "inputs[1].item_ref"},
		{"not UTF-8", "r1\xff", "inputs[1].item_ref"},
		{"a repeat of an earlier label", "ok-0", "inputs[1].item_ref"},
	}
	f := serve(t, 202, submit202)
	a := f.adapter(t)
	for _, c := range cases {
		plan := Prepare(good(c.ref))
		err := plan.Err()
		kind, key := kindAndKey(err)
		if kind != modclient.Invalid || key != c.key {
			t.Errorf("%s: got kind %v key %q (%v), want an Invalid error naming %q", c.name, kind, key, err, c.key)
			continue
		}
		if c.ref != "" && strings.TrimSpace(c.ref) != "" && leaks(err.Error(), c.ref) {
			t.Errorf("%s: the error quotes the label: %q", c.name, err)
		}
		if len(plan.Items) != 0 || len(plan.Malformed) != 0 {
			t.Errorf("%s: a refused plan still lists rows (%d to send, %d malformed)", c.name, len(plan.Items), len(plan.Malformed))
		}
		if _, _, err := a.Submit(testCtx(t), plan); modclient.KindOf(err) != modclient.Invalid {
			t.Errorf("%s: Submit of a refused plan gave %v", c.name, err)
		}
	}
	if f.count() != 0 {
		t.Fatalf("a refused plan still made %d request(s)", f.count())
	}
	// controls: the boundary and the case-sensitive rule
	for _, ref := range []string{strings.Repeat("r", 200), "OK-0", "label with spaces", "é-label"} {
		if err := Prepare(good(ref)).Err(); err != nil {
			t.Errorf("control: the label %q was refused: %v", clip(ref), err)
		}
	}
}

func clip(s string) string {
	if len(s) > 20 {
		return s[:20] + "..."
	}
	return s
}

// TestPrepare_RefusesBatchesModule1WouldRefuse: more rows than module 1's cap, or a body over its limit, is an Invalid error naming the items, made before any request. The numbers
// are module 1's own (100,000 rows, 64 MiB, 200 bytes); the rule is exercised with small limits so no giant body is built, and the cap itself is exercised at its real value
// below. Controls: exactly at the limit is accepted, and the body size the plan computes is exactly the size of the JSON that would be sent.
func TestPrepare_RefusesBatchesModule1WouldRefuse(t *testing.T) {
	if MaxRows != 100_000 || MaxRequestBodyBytes != 64<<20 || MaxItemIDBytes != 200 {
		t.Fatalf("module 1's limits moved: %d rows, %d body bytes, %d item_id bytes", MaxRows, MaxRequestBodyBytes, MaxItemIDBytes)
	}
	if module1Limits != (limits{rows: MaxRows, bodyBytes: MaxRequestBodyBytes}) {
		t.Fatal("Prepare is not using module 1's own limits")
	}
	inputs := []Input{{"a", "ada@acme.example"}, {"b", "bob@acme.example"}, {"c", "cyd@acme.example"}}
	items := []Item{{"a", "ada@acme.example"}, {"b", "bob@acme.example"}, {"c", "cyd@acme.example"}}
	exact, err := json.Marshal(submitBody{Items: items})
	if err != nil {
		t.Fatal(err)
	}
	// rows
	if err := prepare(inputs, limits{rows: 3, bodyBytes: len(exact)}).Err(); err != nil {
		t.Fatalf("control: exactly at both limits was refused: %v", err)
	}
	if kind, key := kindAndKey(prepare(inputs, limits{rows: 2, bodyBytes: len(exact)}).Err()); kind != modclient.Invalid || key != "items" {
		t.Fatalf("one row over the cap: kind %v key %q, want Invalid naming items", kind, key)
	}
	// bytes: the plan's size is the JSON's size, so one byte less refuses it
	err = prepare(inputs, limits{rows: 3, bodyBytes: len(exact) - 1}).Err()
	if kind, key := kindAndKey(err); kind != modclient.Invalid || key != "items" || !strings.Contains(err.Error(), fmt.Sprintf("%d bytes", len(exact))) {
		t.Fatalf("a body one byte over the limit: got %v, want an Invalid error that says the body would be %d bytes", err, len(exact))
	}
	// malformed rows are not sent, so they count for neither the rows nor the bytes
	withBad := append([]Input{{"x", "nope"}}, inputs...)
	if err := prepare(withBad, limits{rows: 3, bodyBytes: len(exact)}).Err(); err != nil {
		t.Fatalf("a malformed row counted against the limits: %v", err)
	}
}

// TestPrepare_TheRowCapIsModule1sAtItsRealValue: 100,000 rows are accepted and 100,001 are refused (rows are counted after the malformed ones are taken out). Control: the 100,001st
// row being malformed leaves the plan at 100,000 and accepted.
func TestPrepare_TheRowCapIsModule1sAtItsRealValue(t *testing.T) {
	build := func(n int) []Input {
		in := make([]Input, n)
		for i := range in {
			in[i] = Input{ItemRef: fmt.Sprintf("r%d", i), Address: fmt.Sprintf("u%d@acme.example", i)}
		}
		return in
	}
	if err := Prepare(build(MaxRows)).Err(); err != nil {
		t.Fatalf("exactly %d rows were refused: %v", MaxRows, err)
	}
	if kind, key := kindAndKey(Prepare(build(MaxRows + 1)).Err()); kind != modclient.Invalid || key != "items" {
		t.Fatalf("%d rows: kind %v key %q, want Invalid naming items", MaxRows+1, kind, key)
	}
	extra := append(build(MaxRows), Input{ItemRef: "bad", Address: "no-at"})
	p := Prepare(extra)
	if err := p.Err(); err != nil || len(p.Items) != MaxRows || len(p.Malformed) != 1 {
		t.Fatalf("control: %d item(s), %d malformed, %v", len(p.Items), len(p.Malformed), err)
	}
}

// TestSubmit_NothingToSendIsRefusedNotSubmitted: an empty plan, and a plan whose every row is malformed, are Invalid errors with no request (module 1 would answer an empty list
// with a 400, and a "successful" empty job would be a job id for nothing); the malformed rows are still there to settle. Control: a plan with one good row is submitted.
func TestSubmit_NothingToSendIsRefusedNotSubmitted(t *testing.T) {
	f := serve(t, 202, `{"job_id":"`+testJob+`","status":"accepted","counts":{"total":1,"enqueued":1,"duplicate":0,"malformed":0}}`)
	a := f.adapter(t)
	for name, plan := range map[string]Plan{"empty": Prepare(nil), "all malformed": Prepare([]Input{{"a", "nope"}, {"b", "also nope"}})} {
		id, counts, err := a.Submit(testCtx(t), plan)
		if modclient.KindOf(err) != modclient.Invalid || id != "" || counts != (SubmitCounts{}) {
			t.Errorf("%s: got %q %+v %v, want an Invalid error and no value", name, id, counts, err)
		}
	}
	if f.count() != 0 {
		t.Fatalf("%d request(s) for nothing to send", f.count())
	}
	if n := len(Prepare([]Input{{"a", "nope"}, {"b", "also nope"}}).MalformedSettleItems()); n != 2 {
		t.Fatalf("the all-malformed plan lists %d settle item(s), want 2", n)
	}
	if _, _, err := a.Submit(testCtx(t), Prepare([]Input{{"a", "ada@acme.example"}})); err != nil {
		t.Fatalf("control: %v", err)
	}
}

// TestPrepare_AnAddressModuleOneWouldAnswerInvalidSyntaxIsNeverSentAndIsFree is the half of owner ruling OD-9 (Phase 3 close-out A5) that makes row M1-10 unreachable from the platform:
// an address that normalises under norm/v1 but fails module 1's syntax layer (a local part over 64 bytes, a domain over 255 bytes, a domain with no dot) is filtered by Prepare exactly
// like a malformed one: counted with its reason, never sent, free by P-08. The request body holds none of them. The paired control is the boundary address one step inside each limit,
// which IS sent, and a charge-table control: the outcome module 1 WOULD have answered for them (invalid/invalid_syntax) is charged by M1-10, so the filter is what keeps the platform from
// paying for it.
func TestPrepare_AnAddressModuleOneWouldAnswerInvalidSyntaxIsNeverSentAndIsFree(t *testing.T) {
	long := func(n int) string { return strings.Repeat("a", n) }
	inputs := []Input{
		{"ok-64", long(64) + "@acme.example"},                        // the last local part module 1's layer accepts
		{"bad-65", long(65) + "@acme.example"},                       // one byte over
		{"bad-nodot", "ada@localhost"},                               // a one-label domain
		{"ok-dot", "ada@a.b"},                                        // the smallest dotted domain
		{"bad-multibyte", strings.Repeat("é", 33) + "@acme.example"}, // 66 bytes of local part in 33 letters
		{"bad-domain", "ada@" + strings.Repeat("b.", 128) + "c"},     // 257 bytes
	}
	plan := Prepare(inputs)
	if err := plan.Err(); err != nil {
		t.Fatal(err)
	}
	wantItems := []string{"ok-64", "ok-dot"}
	var gotItems []string
	for _, it := range plan.Items {
		gotItems = append(gotItems, it.ItemID)
	}
	if fmt.Sprint(gotItems) != fmt.Sprint(wantItems) {
		t.Fatalf("rows to send %v, want %v", gotItems, wantItems)
	}
	wantBad := []Malformed{{"bad-65", norm.ReasonLocalTooLong}, {"bad-nodot", norm.ReasonDomainNoDot}, {"bad-multibyte", norm.ReasonLocalTooLong}, {"bad-domain", norm.ReasonDomainTooLong}}
	if fmt.Sprint(plan.Malformed) != fmt.Sprint(wantBad) {
		t.Fatalf("rejected rows %v, want %v (in input order, with the syntax reason)", plan.Malformed, wantBad)
	}
	for _, it := range plan.MalformedSettleItems() {
		if d := charge.Decide(it.Outcome); d.Kind != charge.Free || d.Rule != "P-08" {
			t.Errorf("rejected row %s decides %+v, want Free by P-08", it.ItemRef, d)
		}
	}
	// the control behind the claim: the answer module 1 gives such an address IS charged, so only the filter prevents the charge
	if d := charge.Decide(charge.Outcome{Module: "m1", State: "invalid", Reason: "invalid_syntax"}); d.Kind != charge.Charge || d.Rule != "M1-10" {
		t.Fatalf("control: module 1's own invalid_syntax answer decides %+v, want a charge by M1-10", d)
	}
	f := serve(t, 202, `{"job_id":"`+testJob+`","status":"accepted","counts":{"total":2,"enqueued":2,"duplicate":0,"malformed":0},"malformed_rows":[],"malformed_rows_truncated":false,"processing_paused":false}`)
	if _, _, err := f.adapter(t).Submit(testCtx(t), plan); err != nil {
		t.Fatal(err)
	}
	calls := f.seen()
	body := ""
	if len(calls) > 0 {
		body = string(calls[len(calls)-1].Body)
	}
	if len(calls) != 1 || strings.Contains(body, "bad-") || !strings.Contains(body, "ok-64") || !strings.Contains(body, "ok-dot") {
		t.Fatalf("the request body is %.200s: it must hold the two good rows and none of the rejected ones", body)
	}
}
