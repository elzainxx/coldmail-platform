package m1

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/coldmail-platform/platform/internal/charge"
	"github.com/coldmail-platform/platform/internal/modclient"
	"github.com/coldmail-platform/platform/internal/money"
	"github.com/coldmail-platform/platform/internal/norm"
)

// Module 1's own limits on one bulk request (control-plane/internal/api/api.go and bulk_rows.go at the pinned commit).
const (
	// MaxRows is the built-in cap on the rows of one submission (api.go maxBulkAddresses). An operator can LOWER it with TM_QUEUE_BULK_MAX_ADDRESSES; module 1 then answers 413
	// payload_too_large, which reaches the caller as a Refused error with that code (never a silent truncation), so this constant is the ceiling and not a promise.
	MaxRows = 100_000
	// MaxRequestBodyBytes is the largest request body module 1 reads (bulk_rows.go maxBulkBodyBytes).
	MaxRequestBodyBytes = 64 << 20
	// MaxItemIDBytes is the longest item_id module 1 takes (store.MaxItemIDBytes).
	MaxItemIDBytes = 200
)

// Input is one row the caller wants checked: its own label (becomes module 1's item_id and the ledger's item_ref) and the address as the user gave it.
type Input struct {
	ItemRef string
	Address string
}

// Item is one row of the bulk request body: the shape module 1 takes (`items`), each row with the caller's own label.
type Item struct {
	ItemID  string `json:"item_id"`
	Address string `json:"address"`
}

// Malformed is a row that failed norm/v1: module 1 would answer it with invalid/invalid_syntax, so the platform never sends it. Reason is the machine word of the refusal.
type Malformed struct {
	ItemRef string
	Reason  norm.Reason
}

// Plan is what Prepare decided: the rows to send, the rows that are never sent, and the counts module 1 should report for the former. It is a value: it holds no connection and
// Submit does not change it.
type Plan struct {
	// Items are the rows to send, in input order, each with item_id = the input's ItemRef and the address AS GIVEN (module 1 normalises it itself, by the same frozen rule).
	// Duplicates (the same normalised address) are all here: module 1 answers every row and marks each repeat duplicate_of the first.
	Items []Item
	// Malformed are the rows that failed norm/v1, in input order. They are counted here and never sent.
	Malformed []Malformed
	// Duplicates is how many of Items repeat the normalised address of an earlier item.
	Duplicates int

	refusal *modclient.Error
}

// Err is why the batch cannot be submitted at all (an unusable item_ref, too many rows, a body over module 1's limit), or nil. It is an Invalid error naming the offending input.
// Submit returns it before any request; a caller that wants to know earlier asks here.
func (p Plan) Err() error {
	if p.refusal == nil {
		return nil
	}
	return p.refusal
}

// Rows is the number of inputs the plan covers: the rows to send plus the malformed ones.
func (p Plan) Rows() int { return len(p.Items) + len(p.Malformed) }

// Expected is what module 1's 202 must say about the rows that are sent: every row is well formed (norm/v1 is module 1's own rule, so none can be malformed there), the repeats are
// duplicates, and total = enqueued + duplicate over rows. Submit compares the 202 with it and calls any difference a contract error.
func (p Plan) Expected() SubmitCounts {
	return SubmitCounts{Total: len(p.Items), Enqueued: len(p.Items) - p.Duplicates, Duplicate: p.Duplicates}
}

// MalformedOutcome is the outcome of a row that was never sent: platform / malformed, which the charge table rules free (row P-08).
func MalformedOutcome() charge.Outcome { return charge.Outcome{Module: "platform", State: "malformed"} }

// MalformedSettleItems are the malformed rows as money.SettleItem, ready to settle for 0 credits (charge.Decide answers Free, rule P-08). A caller with more than
// money.MaxSettleItems of them settles them in pages.
func (p Plan) MalformedSettleItems() []money.SettleItem {
	out := make([]money.SettleItem, len(p.Malformed))
	for i, m := range p.Malformed {
		out[i] = money.SettleItem{ItemRef: m.ItemRef, Outcome: MalformedOutcome()}
	}
	return out
}

// limits are the three numbers Prepare and SubmitRaw judge a batch by; the package passes module 1's own, a test passes small ones.
type limits struct {
	rows, bodyBytes int
}

var module1Limits = limits{rows: MaxRows, bodyBytes: MaxRequestBodyBytes}

// Prepare decides, without any network call, what to send of the inputs. Every address goes through norm/v1; one that fails it is NEVER sent and is returned in Plan.Malformed with
// its reason (its outcome is the free P-08 one). The others become Items. A batch module 1 would refuse is refused here instead (Plan.Err, an Invalid error): an unusable item_ref
// on ANY input (the ledger keys a charge by it, so a malformed row needs one too), more than MaxRows rows to send, or a body over MaxRequestBodyBytes.
func Prepare(inputs []Input) Plan { return prepare(inputs, module1Limits) }

func prepare(inputs []Input, lim limits) Plan {
	refs := make([]string, len(inputs))
	for i, in := range inputs {
		refs[i] = in.ItemRef
	}
	if e := checkRefs(refs, "inputs", "item_ref"); e != nil {
		return Plan{refusal: e}
	}
	var p Plan
	seen := make(map[string]struct{}, len(inputs))
	bodyBytes := len(`{"items":[]}`)
	for _, in := range inputs {
		n, err := norm.Normalize(in.Address)
		if err != nil {
			reason, _ := norm.ReasonOf(err)
			p.Malformed = append(p.Malformed, Malformed{ItemRef: in.ItemRef, Reason: reason})
			continue
		}
		if _, repeat := seen[n]; repeat {
			p.Duplicates++
		} else {
			seen[n] = struct{}{}
		}
		it := Item{ItemID: in.ItemRef, Address: in.Address}
		p.Items = append(p.Items, it)
		bodyBytes += itemBytes(it)
	}
	if len(p.Items) > 1 {
		bodyBytes += len(p.Items) - 1 // the commas between rows
	}
	if e := checkLimits(len(p.Items), bodyBytes, lim); e != nil {
		return Plan{refusal: e}
	}
	return p
}

// itemBytes is the size of one row in the request body: exactly what encoding/json (and so modclient) writes for it.
func itemBytes(it Item) int {
	b, err := json.Marshal(it)
	if err != nil {
		return 0 // two strings always encode; unreachable
	}
	return len(b)
}

// checkLimits refuses a batch over module 1's row cap or body limit.
//
// MODULE-FORCED(M1-F10): module 1 answers more than 100,000 rows with a 413 only after reading the whole body, and an operator can lower the cap, so the adapter refuses the ceiling
// locally with a typed Invalid error rather than uploading a body it knows will be refused.
//
// MODULE-FORCED(M1-F11): module 1 answers a body over 64 MiB with 400 validation_failed `body: malformed` (a decode error), not a 413 and not a word that says "too large", so
// the adapter measures the body it would send and refuses locally with a message that says what is wrong.
func checkLimits(rows, bodyBytes int, lim limits) *modclient.Error {
	switch {
	case rows > lim.rows:
		e := newErr(modclient.Invalid, routeSubmit, fmt.Sprintf("%d rows to send, module 1 takes at most %d in one bulk request: submit the batch in parts", rows, lim.rows))
		e.Key = "items"
		return e
	case bodyBytes > lim.bodyBytes:
		e := newErr(modclient.Invalid, routeSubmit, fmt.Sprintf("the request body would be %d bytes, module 1 reads at most %d: submit the batch in parts", bodyBytes, lim.bodyBytes))
		e.Key = "items"
		return e
	}
	return nil
}

// checkRefs refuses the first of refs that module 1 (or the ledger) would not take as an item_id, naming it as <list>[i].<field>: blank, over MaxItemIDBytes bytes, holding a
// control character, not valid UTF-8 (money.Settle refuses it too), or a repeat of an earlier one (compared exactly, case-sensitive, by bytes).
//
// MODULE-FORCED(M1-F9): module 1 answers a bad item_id with a 400 whose details name the row, rejects the WHOLE job for one bad label, and requires every label to be unique within
// the request, so the adapter checks every label first and refuses locally.
func checkRefs(refs []string, list, field string) *modclient.Error {
	first := make(map[string]int, len(refs))
	for i, r := range refs {
		why := ""
		switch {
		case strings.TrimSpace(r) == "":
			why = "is blank"
		case len(r) > MaxItemIDBytes:
			why = fmt.Sprintf("is %d bytes, module 1 takes at most %d", len(r), MaxItemIDBytes)
		case !utf8.ValidString(r):
			why = "is not valid UTF-8"
		case strings.IndexFunc(r, unicode.IsControl) >= 0:
			why = "holds a control character"
		default:
			if j, dup := first[r]; dup {
				why = fmt.Sprintf("repeats the %s of row %d (they are unique within one request, compared exactly)", field, j)
			} else {
				first[r] = i
			}
		}
		if why != "" {
			e := newErr(modclient.Invalid, routeSubmit, fmt.Sprintf("the %s of row %d %s", field, i, why))
			e.Key = fmt.Sprintf("%s[%d].%s", list, i, field)
			return e
		}
	}
	return nil
}
