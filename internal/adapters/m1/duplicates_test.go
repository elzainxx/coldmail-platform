package m1

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/charge"
	"github.com/coldmail-platform/platform/internal/money"
	"github.com/coldmail-platform/platform/internal/norm"
)

// chargeViolations says what is wrong with the settlement of one batch, given the inputs, the settle items built for it (module 1's rows and the plan's malformed rows) and the
// decision charge.Decide makes for each: every input answered exactly once and keyed by its own ref; at most one CHARGE per normalised address (the first row carries it, a repeat
// is free); a malformed row is free.
func chargeViolations(inputs []Input, items []money.SettleItem) []string {
	var out []string
	byRef := map[string]money.SettleItem{}
	for _, it := range items {
		if _, dup := byRef[it.ItemRef]; dup {
			out = append(out, "the ref "+it.ItemRef+" is answered twice")
		}
		byRef[it.ItemRef] = it
	}
	charged := map[string][]string{}
	for _, in := range inputs {
		it, ok := byRef[in.ItemRef]
		if !ok {
			out = append(out, "the row "+in.ItemRef+" has no answer")
			continue
		}
		d := charge.Decide(it.Outcome)
		n, err := norm.Normalize(in.Address)
		if err != nil {
			if d.Kind != charge.Free || d.Rule != "P-08" {
				out = append(out, fmt.Sprintf("the malformed row %s decides %+v, want Free by P-08", in.ItemRef, d))
			}
			continue
		}
		if d.Kind == charge.Charge {
			charged[n] = append(charged[n], in.ItemRef)
		}
	}
	if len(byRef) != len(inputs) {
		out = append(out, fmt.Sprintf("%d answer(s) for %d input(s)", len(byRef), len(inputs)))
	}
	for addr, refs := range charged {
		if len(refs) > 1 {
			out = append(out, fmt.Sprintf("the address %s is charged %d times (%s)", addr, len(refs), strings.Join(refs, ", ")))
		}
	}
	sort.Strings(out)
	return out
}

// TestPlanAndRecordedResults_ADuplicateAddressIsOneChargeAndEveryRowIsAnswered: five inputs, among them the same address three times in three spellings and one malformed row. The plan
// sends four rows (the malformed one never leaves) and predicts one duplicate-pair of repeats; module 1's recorded answer is every row once, the repeats marked duplicate_of the
// first; the settle items of the drained rows plus the plan's malformed row are keyed by exactly the five input refs, and charge.Decide charges the address ONCE (the first row,
// unit m1_own) while the repeats are free by P-07 and the malformed row free by P-08. Paired control: the same recorded answer with duplicate_of removed from one repeat (a
// module that stopped marking them) charges the address twice and chargeViolations says so, so the property is one the data could have broken.
func TestPlanAndRecordedResults_ADuplicateAddressIsOneChargeAndEveryRowIsAnswered(t *testing.T) {
	inputs := []Input{
		{"a1", "ada@acme.example"},
		{"b2", "bob@acme.example"},
		{"a3", "ADA@Acme.Example"},
		{"m4", "no-at-sign.example"},
		{"a5", " ada@acme.example "},
	}
	plan := Prepare(inputs)
	if err := plan.Err(); err != nil {
		t.Fatal(err)
	}
	if want := (SubmitCounts{Total: 4, Enqueued: 2, Duplicate: 2}); plan.Expected() != want || len(plan.Malformed) != 1 {
		t.Fatalf("plan expects %+v with %d malformed, want %+v and 1", plan.Expected(), len(plan.Malformed), want)
	}

	// module 1's answer for the four rows that were sent, ordinals 0..3 (the malformed row was never sent, so it has no ordinal)
	elems := func(markRepeats bool) []string {
		rep := func(ordinal int, ref string) string {
			if markRepeats {
				return element("valid", "accepted", ref, ordinal, `"duplicate_of":0`)
			}
			return element("valid", "accepted", ref, ordinal, "")
		}
		return []string{
			element("valid", "accepted", "a1", 0, ""),
			element("valid", "accepted", "b2", 1, ""),
			rep(2, "a3"),
			rep(3, "a5"),
		}
	}
	settle := func(markRepeats bool) []money.SettleItem {
		f := newFakeM1(t, func(c call) (int, string) {
			if strings.HasSuffix(c.Path, "/results") {
				return 200, page(testJob, "", elems(markRepeats)...)
			}
			return 200, completeBody(4)
		})
		var rows []Row
		if _, err := f.adapter(t).Drain(testCtx(t), testJob, func(r []Row) error { rows = append(rows, r...); return nil }); err != nil {
			t.Fatal(err)
		}
		items, err := ToSettleItems(rows)
		if err != nil {
			t.Fatal(err)
		}
		return append(items, plan.MalformedSettleItems()...)
	}

	items := settle(true)
	if v := chargeViolations(inputs, items); len(v) != 0 {
		t.Fatalf("%v", v)
	}
	rules := map[string]string{}
	for _, it := range items {
		rules[it.ItemRef] = charge.Decide(it.Outcome).Rule
	}
	want := map[string]string{"a1": "M1-03", "b2": "M1-03", "a3": "P-07", "a5": "P-07", "m4": "P-08"}
	if fmt.Sprint(rules) != fmt.Sprint(want) {
		t.Fatalf("the rules that decided were %v, want %v", rules, want)
	}
	if d := charge.Decide(items[0].Outcome); d.Kind != charge.Charge || d.Unit != "m1_own" {
		t.Fatalf("the first row of the address carries the charge: %+v", d)
	}

	// control: module 1 stops marking repeats: the address is charged three times and the check says so
	bad := chargeViolations(inputs, settle(false))
	if len(bad) != 1 || !strings.Contains(bad[0], "ada@acme.example is charged 3 times") {
		t.Fatalf("control: unmarked repeats gave %v, want the address charged 3 times and nothing else", bad)
	}
	// control: a row that goes unanswered is also a violation
	if v := chargeViolations(inputs, items[:len(items)-1]); len(v) == 0 {
		t.Fatal("control: a missing answer was not reported")
	}
}
