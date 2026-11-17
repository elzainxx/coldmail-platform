package m1

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/coldmail-platform/platform/internal/charge"
	"github.com/coldmail-platform/platform/internal/modclient"
	"github.com/coldmail-platform/platform/internal/money"
)

// ErrNoItemID is the cause of a result with no item_id (absent, empty or blank). The platform always submits the `items` shape, so a result without one cannot be attributed to an
// input row and is never settled: the ledger's item_ref is the module's id, never a position or a guess.
var ErrNoItemID = errors.New("m1: a result carries no item_id, so it cannot be attributed to an input row")

// Row is one element of GET /v1/jobs/{id}/results: the /v1/verify verdict object plus the row's identity. Only the fields the platform acts on are kept.
type Row struct {
	// Ordinal is the row's 0-based position in the submitted list (malformed rows counted). Always present.
	Ordinal int
	// ItemID is the caller's label for the row: the ItemRef the platform sent. Never blank in a Row Results returns.
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

// Outcome is what the charge table reads of the row. A row that repeats an earlier row's address (DuplicateOf set) is the platform's own outcome, platform / duplicate: the first
// row of the address carries the charge, so a repeat is free (charge-table rule P-07). Every other row is module 1's own words. charge.Decide is NOT called here: money.Settle
// decides, inside the transaction that writes the ledger.
func (r Row) Outcome() charge.Outcome {
	// MODULE-FORCED(M1-F5): module 1 answers EVERY row of a job, including each repeat of an address, with the first row's verdict (a repeat shares its work item) and marks it only
	// by duplicate_of, so a repeat looks like a full answer; the adapter maps it to the platform's duplicate outcome so the one check is charged once, on the first row.
	if r.DuplicateOf != nil {
		return charge.Outcome{Module: "platform", State: "duplicate"}
	}
	return charge.Outcome{Module: "m1", State: r.Status, Reason: r.Reason, FromCache: r.FromCache, SkipReason: r.SkipReason, FailureOrigin: r.FailureOrigin}
}

// ToSettleItems maps rows to what money.Settle takes. The item_ref is the module's item_id (never the ordinal, never an address). A row without an item_id fails the whole call with
// a Contract error (cause ErrNoItemID) and nothing is returned: a page that cannot be keyed completely is not settled in part.
func ToSettleItems(rows []Row) ([]money.SettleItem, error) {
	out := make([]money.SettleItem, 0, len(rows))
	for i, r := range rows {
		if strings.TrimSpace(r.ItemID) == "" {
			e := contractErr(routeResults, nil, "rows["+strconv.Itoa(i)+"].item_id", fmt.Sprintf("a result carries no item_id (ordinal %d)", r.Ordinal))
			e.Cause = ErrNoItemID
			return nil, e
		}
		out = append(out, money.SettleItem{ItemRef: r.ItemID, Outcome: r.Outcome()})
	}
	return out, nil
}

// wireRow is one element of the results listing as it is decoded (after resultsShape accepted it): the keys Row keeps.
type wireRow struct {
	Ordinal     int    `json:"ordinal"`
	ItemID      string `json:"item_id"`
	DuplicateOf *int   `json:"duplicate_of"`
	Status      string `json:"status"`
	Reason      string `json:"reason"`
	Flags       struct {
		FromCache bool `json:"from_cache"`
	} `json:"flags"`
	Signals struct {
		SMTPSkippedReason string `json:"smtp_skipped_reason"`
		FailureOrigin     string `json:"failure_origin"`
	} `json:"signals"`
}

// rowOf judges one decoded element and keeps what the platform acts on, or says which key is wrong. Every refusal is about a key, never a value (a module that echoes an address into a
// field gets none of it into an error).
func rowOf(i int, w wireRow) (Row, string, string) {
	at := func(k string) string { return "results[" + strconv.Itoa(i) + "]." + k }
	switch {
	case w.Ordinal < 0:
		return Row{}, at("ordinal"), "the ordinal is negative"
	case !knownStatus(w.Status):
		return Row{}, at("status"), "the status is not one of module 1's four"
	case !knownReason(w.Reason):
		return Row{}, at("reason"), "the reason is not one of module 1's vocabulary"
	case strings.TrimSpace(w.ItemID) == "":
		return Row{}, at("item_id"), "the item_id is blank, so the result cannot be attributed to an input row"
	case w.DuplicateOf != nil && (*w.DuplicateOf < 0 || *w.DuplicateOf >= w.Ordinal):
		return Row{}, at("duplicate_of"), "duplicate_of does not point at an earlier row"
	// MODULE-FORCED(M1-F7): the two facts the charge table needs about WHO failed live in the open `signals` bag, not in a field of their own: failure_origin is present on every
	// unknown (and on a risky answer that is our own degraded fallback) and absent elsewhere, smtp_skipped_reason only where the SMTP stage did not run, so an absent key is a
	// normal answer ("" = the table's `none`), a present one must be in the closed vocabulary, and an unknown with no origin is left to the table (M1-23: unmapped, never charged).
	case w.Signals.FailureOrigin != "" && !knownOrigin(w.Signals.FailureOrigin):
		return Row{}, at("signals.failure_origin"), "the failure origin is not own, remote or undetermined"
	case w.Signals.SMTPSkippedReason != "" && !knownSkipToken(w.Signals.SMTPSkippedReason):
		return Row{}, at("signals.smtp_skipped_reason"), "the SMTP skip token is not one of module 1's"
	}
	return Row{
		Ordinal: w.Ordinal, ItemID: w.ItemID, DuplicateOf: w.DuplicateOf, Status: w.Status, Reason: w.Reason,
		// MODULE-FORCED(M1-F6): the cache signal is flags.from_cache; meta.from_cache is ALWAYS false in a bulk result (the bulk meta is skeletal), so reading the obvious key would
		// bill every cached answer as a fresh one.
		FromCache:     w.Flags.FromCache,
		SkipReason:    w.Signals.SMTPSkippedReason,
		FailureOrigin: w.Signals.FailureOrigin,
	}, "", ""
}

// Results reads one page of the job's results, in ordinal order: GET /v1/jobs/{id}/results, idempotent, retried per the dials. cursor is "" for the first page, else the next
// returned by the previous page, passed back unchanged (it is opaque). limit is 1..MaxPageLimit, or 0 for module 1's default; anything else is refused here. next is "" when the list
// is exhausted AS IT STANDS: a job that is still running can settle more rows later (see Drain). A page with one element that is not on contract fails the WHOLE page: partial rows
// are never returned.
func (a *Adapter) Results(ctx context.Context, id, cursor string, limit int) (rows []Row, next string, err error) {
	if e := checkJobID(routeResults, id); e != nil {
		return nil, "", e
	}
	// MODULE-FORCED(M1-F12): module 1 answers a limit that is not 1..1000 (0, negative, not a number, above 1000) with ITS OWN DEFAULT of 100, silently and with no error, so a caller
	// who asked for 5000 would believe it had read pages of 5000; the adapter refuses what the module would silently change.
	if limit < 0 || limit > MaxPageLimit {
		return nil, "", newErr(modclient.Invalid, routeResults, fmt.Sprintf("the limit must be 0 (the module's default) or 1..%d: module 1 would silently use its default of %d", MaxPageLimit, DefaultPageLimit))
	}
	q := url.Values{}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	resp, err := a.c.Do(ctx, modclient.Request{
		Module: modclient.M1, Route: routeResults, Method: http.MethodGet, Path: "/v1/jobs/" + url.PathEscape(id) + "/results", Query: q, Idempotent: true,
	})
	if err != nil {
		return nil, "", err
	}
	var w struct {
		JobID      string    `json:"job_id"`
		Results    []wireRow `json:"results"`
		NextCursor string    `json:"next_cursor"`
	}
	if err := modclient.Decode(resp, &w, resultsShape); err != nil {
		return nil, "", err
	}
	if !strings.EqualFold(w.JobID, id) {
		return nil, "", contractErr(routeResults, resp, "job_id", "the page is about another job")
	}
	rows = make([]Row, 0, len(w.Results))
	for i, wr := range w.Results {
		r, key, why := rowOf(i, wr)
		if key != "" {
			return nil, "", contractErr(routeResults, resp, key, why)
		}
		rows = append(rows, r)
	}
	return rows, w.NextCursor, nil
}
