package m3

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// FieldIssue is one field-level problem the module named for a rejected row: the module's own fixed words (a field name of the request and `required`, `malformed`, `out_of_range`,
// `not_found` or `conflict`), kept only when they are plain words.
type FieldIssue struct {
	Field string
	Issue string
}

// BulkRow is the outcome of one row of a bulk add, in the order the rows were sent (Row is 1-based).
type BulkRow struct {
	Row int
	// Created is true when the module created the account (it exists, verified or not); false when it rejected the row and created nothing.
	Created bool

	// For a created row:
	AccountID string
	// Verification is the credential check of the new account: verified, failed or unverified.
	Verification VerificationState
	// VerifyCode says why a failed check failed ("" otherwise).
	VerifyCode   VerifyCode
	SMTPResponse *string
	// Alignment is the From-gate verdict ("" when the module gave none, for instance when the DNS did not answer); AlignmentMessage the module's sentence for it.
	Alignment        AlignmentState
	AlignmentMessage *string

	// For a rejected row: Reason is one of the module's closed set (RejectReason; since Phase 3 close-out C3 an internal reason never crosses the boundary: the module logs it and answers its
	// external code), and Fields the field problems the module named.
	Reason RejectReason
	Fields []FieldIssue
}

// HostForbidden is true for a rejected row whose smtp_host the module's destination guard refused: validation_failed with {smtp_host, forbidden_value} in its details (Phase 3 close-out C4).
// The other rows of the batch are not affected.
func (r BulkRow) HostForbidden() bool {
	if r.Created || r.Reason != RejectValidation {
		return false
	}
	for _, f := range r.Fields {
		if f.Field == "smtp_host" && f.Issue == "forbidden_value" {
			return true
		}
	}
	return false
}

// Err is the typed refusal of a row the module rejected because of its SMTP host (ErrHostForbidden, also a Refused modclient error), and nil for every other row. The row's own reason and
// fields are the other facts about a rejection.
func (r BulkRow) Err() error {
	if !r.HostForbidden() {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrHostForbidden, &modclient.Error{Kind: modclient.Refused, Module: modclient.M3, Route: routeBulk, Status: http.StatusBadRequest, Message: "the module refused the row's SMTP host as a destination", Sent: true})
}

// BulkResult is the answer of a bulk add: one BulkRow per row sent, in order, or an error and nothing.
type BulkResult struct {
	// RequestID is the module's id of the request, for its log.
	RequestID string
	Rows      []BulkRow
}

var bulkShape = modclient.Object(
	modclient.Req("request_id", modclient.Str),
	modclient.Req("results", modclient.ArrayOf(modclient.Object(
		modclient.Req("row", modclient.Int), modclient.Req("status", modclient.Str),
		modclient.Opt("account_id", modclient.Str), modclient.Opt("verification", modclient.Str), modclient.Opt("code", modclient.Str), modclient.Opt("smtp_response", modclient.Str),
		modclient.Opt("from_alignment_state", modclient.Str), modclient.Opt("from_alignment_message", modclient.Str),
		modclient.Opt("details", modclient.ArrayOf(modclient.Object(modclient.Req("field", modclient.Str), modclient.Req("issue", modclient.Str)))),
	))),
)

// safeIssueWord is the alphabet of the module's fixed field and issue words.
func safeIssueWord(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}

// BulkAddAccounts adds several accounts for the tenant in ONE synchronous request (POST /v1/send-byo/accounts/bulk) and returns one outcome per row. The module paces its checks (a pause
// between rows) and verifies each row, so the request is long: its budget is BulkTimeout(len(accounts)), and a call that times out has an unknown outcome per row (MaybeApplied; list the
// accounts to find out which rows were created). It is never retried by the adapter. At most MaxBulkRows rows; each row obeys the same host rule as CreateAccount, and one row that
// breaks it refuses the whole batch before anything is sent.
//
// A 409 (the whole batch is refused for a suspended user) is ErrActionUnavailable; a batch over the module's row limit is a 422 Refused error. A row whose host the module's destination guard
// refuses is rejected alone (BulkRow.HostForbidden); the other rows are processed. The answer is checked against what was sent: exactly one result per row, in order, each created row naming
// an account id, each status one of created or rejected, every state and code one of the module's closed sets (Phase 3 close-out C3); anything else is a Contract error naming the key
// and no rows are returned, because a listing that may be missing rows is never returned as complete.
func (c *Client) BulkAddAccounts(ctx context.Context, accounts []NewAccount) (BulkResult, error) {
	switch {
	case len(accounts) == 0:
		return BulkResult{}, invalid(routeBulk, "the batch has no accounts")
	case len(accounts) > MaxBulkRows:
		return BulkResult{}, invalid(routeBulk, fmt.Sprintf("the batch has more than %d accounts; split it", MaxBulkRows))
	}
	rows := make([]wireNew, len(accounts))
	for i, n := range accounts {
		if err := c.check(routeBulk, n); err != nil {
			return BulkResult{}, fmt.Errorf("row %d: %w", i+1, err)
		}
		rows[i] = n.wire()
	}
	body := struct {
		UserID   string    `json:"user_id"`
		Accounts []wireNew `json:"accounts"`
	}{c.UserID(), rows}
	resp, err := c.Module().Do(ctx, modclient.Request{
		Module: modclient.M3, Route: routeBulk, Method: http.MethodPost, Path: pathAccounts + "/bulk", Body: body, Timeout: BulkTimeout(len(accounts)),
	})
	if err != nil {
		return BulkResult{}, accountErr(routeBulk, err)
	}
	var w struct {
		RequestID string `json:"request_id"`
		Results   []struct {
			Row          int     `json:"row"`
			Status       string  `json:"status"`
			AccountID    string  `json:"account_id"`
			Verification string  `json:"verification"`
			Code         string  `json:"code"`
			SMTPResponse *string `json:"smtp_response"`
			Alignment    string  `json:"from_alignment_state"`
			AlignMessage *string `json:"from_alignment_message"`
			Details      []struct {
				Field string `json:"field"`
				Issue string `json:"issue"`
			} `json:"details"`
		} `json:"results"`
	}
	if err := modclient.Decode(resp, &w, bulkShape); err != nil {
		return BulkResult{}, err
	}
	if len(w.Results) != len(accounts) {
		return BulkResult{}, contract(resp, routeBulk, "results", "the module answered a different number of rows than were sent")
	}
	out := BulkResult{RequestID: w.RequestID, Rows: make([]BulkRow, 0, len(w.Results))}
	for i, r := range w.Results {
		path := "results[" + strconv.Itoa(i) + "]"
		if r.Row != i+1 {
			return BulkResult{}, contract(resp, routeBulk, path+".row", "the rows are not answered in the order they were sent")
		}
		created, ok := bulkStatus(r.Status)
		if !ok {
			return BulkResult{}, contract(resp, routeBulk, path+".status", "the row status is neither created nor rejected, so whether the account exists is not known")
		}
		row := BulkRow{Row: r.Row, Created: created}
		if created {
			if !sendapi.CanonicalUUID(r.AccountID) {
				return BulkResult{}, contract(resp, routeBulk, path+".account_id", "a created row names no account id")
			}
			row.AccountID = r.AccountID
			if row.Verification, ok = testVerificationState(r.Verification); !ok {
				return BulkResult{}, contract(resp, routeBulk, path+".verification", "the verification state is not one of the module's closed set")
			}
			if row.VerifyCode, ok = verifyCode(r.Code); !ok {
				return BulkResult{}, contract(resp, routeBulk, path+".code", "the verification code is not one of the module's closed set")
			}
			row.SMTPResponse = providerText(r.SMTPResponse)
			if r.Alignment != "" {
				if row.Alignment, ok = testAlignmentState(r.Alignment); !ok {
					return BulkResult{}, contract(resp, routeBulk, path+".from_alignment_state", "the From verdict is not one of the module's closed set")
				}
			}
			row.AlignmentMessage = providerText(r.AlignMessage)
		} else {
			if row.Reason, ok = rejectReason(r.Code); !ok {
				return BulkResult{}, contract(resp, routeBulk, path+".code", "the rejection code is not one of the module's closed set")
			}
			for _, d := range r.Details {
				if safeIssueWord(d.Field) && safeIssueWord(d.Issue) {
					row.Fields = append(row.Fields, FieldIssue{Field: d.Field, Issue: d.Issue})
				}
			}
		}
		out.Rows = append(out.Rows, row)
	}
	return out, nil
}

// fixedWord carries one of the module's fixed identifiers (a cap name, a warning code, an actor, an action) only when it is a plain word, and the Unrecognised word otherwise: free text
// the module might put there is not passed on.
func fixedWord(s string) string {
	if s == "" || safeIssueWord(s) {
		return s
	}
	return wordUnrecognised
}
