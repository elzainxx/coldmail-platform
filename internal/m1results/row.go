// Package m1results is the platform's caller of module 1's bulk surface: the first real consumer of its per-item results (Phase 2 close-out D2).
//
// Module 1 answers a bulk job with one result per INPUT ROW, in ordinal order, each carrying the caller's own `item_id` (the platform sends one per row
// at submit), the row's `ordinal` and, on a row that repeats an earlier row's address, `duplicate_of`. That is what lets the platform settle a job item by
// item as results arrive, with the module's own id as the ledger's `item_ref`, instead of waiting for the whole job and guessing a key from a position.
//
// The package holds two small things and no business logic:
//
//   - row.go: Row (one result element, decoded strictly where money depends on it) and ToSettleItems, the mapping from rows to money.SettleItem.
//     charge.Decide is NEVER called here: money.Settle decides, from the outcome this package describes.
//   - client.go: Client (submit, job status, one page of results, and Drain, which pages until the job is complete and hands each page to a callback as
//     it arrives).
//
// The routes and the wire shapes are module 1's own, as documented in its bulk-item-identity.md and api-contract.md; the auth is `Authorization: Bearer
// <per-caller key>` (the same header internal/probe sends to the same routes).
package m1results

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/coldmail-platform/platform/internal/charge"
	"github.com/coldmail-platform/platform/internal/money"
)

// ErrNoItemID is a result with no item_id (absent, empty or blank). The platform always submits the `items` shape, so a result without one cannot be
// attributed to an input row and is never settled: the ledger's item_ref is the module's id, never a position or a guess.
var ErrNoItemID = errors.New("m1results: a result carries no item_id, so it cannot be attributed to an input row")

// ErrBadRow is the root of every error about one result element (see Row.UnmarshalJSON); its text carries ordinals and fixed words only, never a value
// taken from the element.
var ErrBadRow = errors.New("m1results: a result element is malformed")

// Row is one element of GET /v1/jobs/{id}/results: the /v1/verify verdict object plus the row's identity. Only the fields the platform acts on are kept.
type Row struct {
	// Ordinal is the row's 0-based position in the submitted list (malformed rows counted). Always present.
	Ordinal int
	// ItemID is the caller's label for the row. "" when the module sent none.
	ItemID string
	// DuplicateOf is the ordinal of the FIRST row of the job with the same address; nil on a first row and on every malformed row.
	DuplicateOf *int
	// Status and Reason are the verdict (valid, invalid, risky, unknown; and the reason vocabulary).
	Status string
	Reason string
	// FromCache is flags.from_cache (never meta.from_cache, which is always false in a bulk result).
	FromCache bool
	// SkipReason is signals.smtp_skipped_reason ("" = absent).
	SkipReason string
	// FailureOrigin is signals.failure_origin ("" = absent; "own", "remote" or "undetermined" on an unknown).
	FailureOrigin string
}

// UnmarshalJSON decodes one result element. Unknown fields are ignored (the verdict object is an open object and the module adds keys additively). It
// is strict about the two fields every consumer needs: a missing or empty `status` and a missing or negative `ordinal` are errors, and so is a
// `duplicate_of` that does not point at an EARLIER row (the module's own constraint; a row freed as a duplicate of something that is not before it must
// not be believed). A missing item_id is NOT an error here (the legacy `addresses` shape has none); ToSettleItems refuses it.
func (r *Row) UnmarshalJSON(b []byte) error {
	var w struct {
		Status      *string `json:"status"`
		Reason      string  `json:"reason"`
		ItemID      string  `json:"item_id"`
		Ordinal     *int    `json:"ordinal"`
		DuplicateOf *int    `json:"duplicate_of"`
		Flags       struct {
			FromCache bool `json:"from_cache"`
		} `json:"flags"`
		Signals struct {
			SMTPSkippedReason string `json:"smtp_skipped_reason"`
			FailureOrigin     string `json:"failure_origin"`
		} `json:"signals"`
	}
	if err := json.Unmarshal(b, &w); err != nil {
		return fmt.Errorf("%w: it is not a JSON object of the documented types", ErrBadRow)
	}
	switch {
	case w.Ordinal == nil:
		return fmt.Errorf("%w: it has no ordinal", ErrBadRow)
	case *w.Ordinal < 0:
		return fmt.Errorf("%w: it has a negative ordinal (%d)", ErrBadRow, *w.Ordinal)
	case w.Status == nil || strings.TrimSpace(*w.Status) == "":
		return fmt.Errorf("%w: the result of ordinal %d has no status", ErrBadRow, *w.Ordinal)
	case w.DuplicateOf != nil && (*w.DuplicateOf < 0 || *w.DuplicateOf >= *w.Ordinal):
		return fmt.Errorf("%w: the result of ordinal %d says it duplicates ordinal %d, which is not an earlier row", ErrBadRow, *w.Ordinal, *w.DuplicateOf)
	}
	*r = Row{
		Ordinal:       *w.Ordinal,
		ItemID:        w.ItemID,
		DuplicateOf:   w.DuplicateOf,
		Status:        *w.Status,
		Reason:        w.Reason,
		FromCache:     w.Flags.FromCache,
		SkipReason:    w.Signals.SMTPSkippedReason,
		FailureOrigin: w.Signals.FailureOrigin,
	}
	return nil
}

// ToSettleItems maps rows to what money.Settle takes. The item_ref is the module's item_id (never the ordinal, never an address). A row that repeats an
// earlier row's address (duplicate_of set) is the platform's own outcome, platform/duplicate: the first row of the address carries the charge, so a
// repeat is free (charge-table rule P-07). Every other row is module 1's own words: its status and reason, whether the answer came from the cache, the
// SMTP skip token and the failure origin. charge.Decide is not called: money.Settle decides, inside the transaction that writes the ledger.
//
// A row without an item_id fails the whole call with ErrNoItemID and nothing is returned: a page that cannot be keyed completely is not settled in part.
func ToSettleItems(rows []Row) ([]money.SettleItem, error) {
	out := make([]money.SettleItem, 0, len(rows))
	for _, r := range rows {
		if strings.TrimSpace(r.ItemID) == "" {
			return nil, fmt.Errorf("%w (ordinal %d)", ErrNoItemID, r.Ordinal)
		}
		o := charge.Outcome{
			Module:        "m1",
			State:         r.Status,
			Reason:        r.Reason,
			FromCache:     r.FromCache,
			SkipReason:    r.SkipReason,
			FailureOrigin: r.FailureOrigin,
		}
		if r.DuplicateOf != nil {
			o = charge.Outcome{Module: "platform", State: "duplicate"}
		}
		out = append(out, money.SettleItem{ItemRef: r.ItemID, Outcome: o})
	}
	return out, nil
}
