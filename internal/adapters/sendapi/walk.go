package sendapi

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/coldmail-platform/platform/internal/modclient"
)

// ReportCampaignPage reads ONE page of a campaign report (GET /v1/report?campaign_id=...). cursor is "" for the first page and otherwise exactly the NextCursor of the page before it: the
// cursor is opaque and is never built, edited or reused by the adapter. A cursor the module does not accept is ErrBadCursor.
//
// An unknown campaign, a typo and another tenant's campaign are NOT a 404: the module answers 200 with total 0 and no items (CampaignReport.NothingKnown).
func (c *Client) ReportCampaignPage(ctx context.Context, campaignID, cursor string) (CampaignPage, error) {
	if err := c.id(opReportCamp, "campaign id", campaignID); err != nil {
		return CampaignPage{}, err
	}
	if len(cursor) > maxCursorBytes {
		return CampaignPage{}, c.invalid(opReportCamp, "the cursor is longer than any cursor the module issues")
	}
	q := url.Values{"user_id": {c.user}, "campaign_id": {campaignID}, "page_size": {strconv.Itoa(c.opts.PageSize)}}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	resp, err := c.call(ctx, opReportCamp, http.MethodGet, "/v1/report", q, nil)
	if err != nil {
		return CampaignPage{}, c.mapErr(opReportCamp, err, cursor != "")
	}
	var w struct {
		CampaignID     string `json:"campaign_id"`
		DispatchPaused bool   `json:"dispatch_paused"`
		Paused         bool   `json:"paused"` // additive (Phase 4 C1), absent in an older module: false (the shape declares it optional and a present value must be a boolean)
		Totals         struct {
			Total              int64 `json:"total"`
			Sent               int64 `json:"sent"`
			Bounced            int64 `json:"bounced"`
			Failed             int64 `json:"failed"`
			InFlight           int64 `json:"in_flight"`
			Pending            int64 `json:"pending"`
			WaitingForCapacity int64 `json:"waiting_for_capacity"`
			Cancelled          int64 `json:"cancelled"`
		} `json:"totals"`
		Progress struct {
			Terminal         int64   `json:"terminal"`
			FractionComplete float64 `json:"fraction_complete"`
			IsComplete       bool    `json:"is_complete"`
		} `json:"progress"`
		Items struct {
			Results    []wireItem `json:"results"`
			NextCursor *string    `json:"next_cursor"`
			PageSize   int64      `json:"page_size"`
		} `json:"items"`
		ByAccount []struct {
			AccountID       string `json:"account_id"`
			AccountUsername string `json:"account_username"`
			Sent            int64  `json:"sent"`
			Bounced         int64  `json:"bounced"`
			Failed          int64  `json:"failed"`
			InFlight        int64  `json:"in_flight"`
		} `json:"totals_by_account"`
	}
	if err := modclient.Decode(resp, &w, c.sh.page); err != nil {
		return CampaignPage{}, err
	}
	t := w.Totals
	for _, k := range []struct {
		key string
		n   int64
	}{
		{"totals.total", t.Total}, {"totals.sent", t.Sent}, {"totals.bounced", t.Bounced}, {"totals.failed", t.Failed}, {"totals.in_flight", t.InFlight},
		{"totals.pending", t.Pending}, {"totals.waiting_for_capacity", t.WaitingForCapacity}, {"totals.cancelled", t.Cancelled}, {"progress.terminal", w.Progress.Terminal},
	} {
		if k.n < 0 {
			return CampaignPage{}, c.contract(resp, opReportCamp, k.key, "the count is negative")
		}
	}
	if !strings.EqualFold(w.CampaignID, campaignID) {
		return CampaignPage{}, c.contract(resp, opReportCamp, "campaign_id", "the report is for another campaign than the one asked")
	}
	// MODULE-FORCED(SA-F8): the module clamps page_size silently (0, negative or text becomes 100, above 200 becomes 200), so the adapter reads the page size the module says it used and checks the page against it, never against what it asked.
	if w.Items.PageSize < 1 || w.Items.PageSize > MaxPageSize {
		return CampaignPage{}, c.contract(resp, opReportCamp, "items.page_size", "the page size the module says it used is outside 1 to the module's maximum")
	}
	if int64(len(w.Items.Results)) > w.Items.PageSize {
		return CampaignPage{}, c.contract(resp, opReportCamp, "items.results", "the page holds more items than the page size the module says it used")
	}
	next := ""
	if w.Items.NextCursor != nil {
		if *w.Items.NextCursor == "" || len(*w.Items.NextCursor) > maxCursorBytes {
			return CampaignPage{}, c.contract(resp, opReportCamp, "items.next_cursor", "the cursor is empty or longer than any cursor the module issues")
		}
		next = *w.Items.NextCursor
	}
	page := CampaignPage{
		CampaignID: campaignID, DispatchPaused: w.DispatchPaused, Paused: w.Paused, NextCursor: next, PageSize: int(w.Items.PageSize),
		Totals: Totals{
			Total: t.Total, Sent: t.Sent, Bounced: t.Bounced, Failed: t.Failed, InFlight: t.InFlight, Pending: t.Pending,
			WaitingForCapacity: t.WaitingForCapacity, Cancelled: t.Cancelled,
		},
		Progress: Progress{Terminal: w.Progress.Terminal, FractionComplete: w.Progress.FractionComplete, IsComplete: w.Progress.IsComplete},
		Items:    make([]Item, 0, len(w.Items.Results)),
	}
	for i, wi := range w.Items.Results {
		it, err := c.item(resp, opReportCamp, "items.results["+strconv.Itoa(i)+"]", wi, false)
		if err != nil {
			return CampaignPage{}, err
		}
		page.Items = append(page.Items, it)
	}
	for _, a := range w.ByAccount {
		page.ByAccount = append(page.ByAccount, AccountTotals{AccountID: a.AccountID, AccountUsername: a.AccountUsername, Sent: a.Sent, Bounced: a.Bounced, Failed: a.Failed, InFlight: a.InFlight})
	}
	if len(page.Items) == 0 && next != "" {
		return CampaignPage{}, c.contract(resp, opReportCamp, "items.next_cursor", "an empty page names a next cursor, so the cursor does not advance")
	}
	return page, nil
}

// Walk reads every page of a campaign report in the module's order, handing each page to fn as it is read (so a caller may settle page by page: settlement is idempotent per item). It
// returns what it read, or an error and NOTHING: a page that cannot be read ends the walk with that page's typed error, wrapped with how many pages fn had already been given, and the
// listing is not complete. The caller must not treat what fn was handed as the listing unless Walk returned nil.
//
// The walk refuses what would make it loop or lie, each as a Contract error naming the key: a cursor that repeats or does not advance, an empty page that names a next cursor, more than
// Options.MaxPages pages, the same job item on two pages, and a page for another campaign. An error from fn ends the walk and is returned as it is.
//
// A walk is exact only for a campaign that no longer receives run calls, and the totals are not a snapshot with the items (research m2 4.2); settle idempotently per job item and walk again
// until every submitted item is terminal.
func (c *Client) Walk(ctx context.Context, campaignID string, fn func(CampaignPage) error) (WalkStats, error) {
	if fn == nil {
		return WalkStats{}, c.invalid(opReportCamp, "the walk needs a callback")
	}
	var st WalkStats
	cursor := ""
	asked := map[string]bool{}
	seen := map[string]bool{}
	for {
		if st.Pages >= c.opts.MaxPages {
			return WalkStats{}, c.contract(nil, opReportCamp, "items.next_cursor", fmt.Sprintf("the walk did not end within %d pages", c.opts.MaxPages))
		}
		asked[cursor] = true
		page, err := c.ReportCampaignPage(ctx, campaignID, cursor)
		if err != nil {
			return WalkStats{}, fmt.Errorf("sendapi: the walk of the campaign report failed on page %d (%d page(s) before it were delivered; the listing is NOT complete): %w", st.Pages+1, st.Pages, err)
		}
		for _, it := range page.Items {
			if seen[it.JobItemID] {
				return WalkStats{}, c.contract(nil, opReportCamp, "items.results", "the same job item is on two pages, so the cursor does not hold the exact-once order")
			}
			seen[it.JobItemID] = true
		}
		if err := fn(page); err != nil {
			return WalkStats{}, err
		}
		st.Pages++
		st.Items += len(page.Items)
		if page.NextCursor == "" {
			return st, nil
		}
		if asked[page.NextCursor] {
			return WalkStats{}, c.contract(nil, opReportCamp, "items.next_cursor", "the next cursor was already used, so the cursor does not advance")
		}
		cursor = page.NextCursor
	}
}

// ReportCampaign reads the whole campaign report: every page, in order, or an error and no report. It is Walk collected.
func (c *Client) ReportCampaign(ctx context.Context, campaignID string) (CampaignReport, error) {
	var rep CampaignReport
	_, err := c.Walk(ctx, campaignID, func(p CampaignPage) error {
		rep.CampaignID, rep.DispatchPaused, rep.Paused = p.CampaignID, p.DispatchPaused, p.Paused
		// MODULE-FORCED(SA-F9): the totals and the item list are separate queries of the module, not a snapshot, so the report carries the totals of the last page read and the caller checks ItemsMatchTotal instead of the adapter asserting that the two agree.
		rep.Totals, rep.Progress, rep.ByAccount = p.Totals, p.Progress, p.ByAccount
		rep.Items = append(rep.Items, p.Items...)
		rep.Pages++
		return nil
	})
	if err != nil {
		return CampaignReport{}, err
	}
	return rep, nil
}
