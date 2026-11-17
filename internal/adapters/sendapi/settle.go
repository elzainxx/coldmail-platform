package sendapi

import (
	"fmt"
	"strings"

	"github.com/coldmail-platform/platform/internal/charge"
	"github.com/coldmail-platform/platform/internal/money"
)

// maxItemRefBytes is money.SettleItem's limit on an item_ref.
const maxItemRefBytes = 200

// Outcome is what the charge table reads of a reported item: the module (the table's own module column), the state and the module's reason ("" when the module sent none, and for a module
// without a reason key). Nothing else about an item reaches the table: not its attempt count, not its error class, not its provider id.
func (p Profile) Outcome(it Item) charge.Outcome {
	o := charge.Outcome{Module: string(p.Module), State: it.State}
	if it.Reason != nil {
		o.Reason = *it.Reason
	}
	return o
}

// Classify sorts a batch of reported items into what can be settled, what is not final yet and what the charge table cannot classify. The item_ref of each settle item is the platform's own
// job item id (Item.JobItemID, unique in the batch, at most 200 bytes), never a position and never an address.
//
//   - A non-terminal item (queued, assigned, sending) is returned in NotYet and is never an outcome, whatever the charge table says of its state (module 3's table row is "free" for every
//     state: it must not settle an item that has not finished).
//   - A terminal item the table maps to Charge or Free goes to Settle, for money.Settle, which asks the same table inside the transaction that writes the ledger.
//   - A terminal item the table cannot classify (anything but Charge or Free) goes to Unmapped with the rule that said so, and is never charged. So does an item whose state is not in the profile's set, though the adapter's own decoding never produces one.
//
// It refuses the whole batch (and returns the zero Classified) for an item with no job item id or one that is repeated: a batch that cannot be keyed completely is not settled in part.
// The class is recomputed from the state, so a caller that edits Item.Class changes nothing.
func (c *Client) Classify(items []Item) (Classified, error) {
	seen := make(map[string]bool, len(items))
	var out Classified
	for i, it := range items {
		ref := it.JobItemID
		switch {
		case strings.TrimSpace(ref) == "":
			return Classified{}, c.invalid(opReport, fmt.Sprintf("item %d has no job item id, so its outcome cannot be attributed", i))
		case len(ref) > maxItemRefBytes:
			return Classified{}, c.invalid(opReport, fmt.Sprintf("item %d has a job item id longer than %d bytes", i, maxItemRefBytes))
		case seen[ref]:
			return Classified{}, c.invalid(opReport, fmt.Sprintf("item %d repeats the job item id of an earlier item", i))
		}
		seen[ref] = true
		reason := ""
		if it.Reason != nil {
			reason = *it.Reason
		}
		unmapped := func(rule, note string) {
			out.Unmapped = append(out.Unmapped, Unmapped{JobItemID: ref, State: it.State, Reason: reason, Rule: rule, Note: note})
		}
		if !c.p.hasState(it.State) {
			unmapped("none", "the state is not one of the states the module advertises to this adapter")
			continue
		}
		cls, _ := stateClass(it.State) // handled: validate refused a profile state the switch does not
		if cls != ClassTerminal {
			out.NotYet = append(out.NotYet, Pending{JobItemID: ref, State: it.State, Class: cls})
			continue
		}
		switch d := charge.Decide(c.p.Outcome(it)); d.Kind {
		case charge.Charge, charge.Free:
			out.Settle = append(out.Settle, money.SettleItem{ItemRef: ref, Outcome: c.p.Outcome(it)})
		default: // Unmapped (and a NotFinal for a state the profile calls final, which the shipped table never says): flagged with the row that decided, never settled
			unmapped(d.Rule, d.Note)
		}
	}
	return out, nil
}
