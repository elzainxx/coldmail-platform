package m1

import (
	"context"
	"net/http"
	"net/url"

	"github.com/coldmail-platform/platform/internal/modclient"
)

// The three words of `cancel_outcome` in the answer of POST /v1/jobs/{id}/cancel (module 1 ADR-0023; owner ruling C2). A closed set: any other word is a Contract error naming
// cancel_outcome, never a guess.
const (
	// CancelOutcomeCancelled: this call cancelled the job. cancel_requested is now true and every row not yet claimed is `cancelled` (free, no verdict); the rows already leased or in
	// flight finish and settle as verdicts.
	CancelOutcomeCancelled = "cancelled"
	// CancelOutcomeAlreadyCancelled: an earlier call cancelled it (a repeat is never an error and changes nothing).
	CancelOutcomeAlreadyCancelled = "already_cancelled"
	// CancelOutcomeAlreadyComplete: every row was already final, so there was nothing to cancel; the job is NOT marked cancelled (cancel_requested stays false).
	CancelOutcomeAlreadyComplete = "already_complete"
)

// CancelResult is the answer of module 1 to a cancel: the outcome word and the job's status as of the snapshot taken after the cancel (the same body GET /v1/jobs/{id} serves).
type CancelResult struct {
	// Outcome is CancelOutcomeCancelled, CancelOutcomeAlreadyCancelled or CancelOutcomeAlreadyComplete.
	Outcome string
	// Status is the job after the call, validated exactly as Status validates it (totals.submitted = settled + cancelled + pending, ...).
	Status JobStatus
}

// wireCancel is the cancel answer as it is decoded (after cancelShape accepted it): the job-status body and the outcome word.
type wireCancel struct {
	wireStatus
	CancelOutcome string `json:"cancel_outcome"`
}

// Cancel cancels a bulk job: POST /v1/jobs/{id}/cancel with the tenant's key and no body, expect 200. Module 1 makes every row not yet claimed `cancelled` (free, no verdict, nothing sent
// to a vendor) and lets the rows already leased or in flight finish and settle, so after a cancel the job completes once those settle; it never takes the job back out of the queue
// for rows it holds. The route is IDEMPOTENT (a repeat is `already_cancelled`, a cancel of a finished job `already_complete`, neither an error), so it is retried per the dials like a read.
//
// The answer is the job-status body plus `cancel_outcome`, decoded with the SAME validation Status applies (judgeStatus): a body whose totals contradict themselves is a Contract
// error, and so is an outcome word outside the three. A job of another caller is the 404 of an absent job (NotFound), an id that is not a uuid is refused before any request (Invalid), a
// store that cannot cancel answers 503 unavailable (Unavailable): every error is the typed modclient error of the route, the same classification as the other routes. ON ANY ERROR THE
// RESULT IS THE ZERO CancelResult: the platform never reads a half-answered cancel as "cancelled".
func (a *Adapter) Cancel(ctx context.Context, jobID string) (CancelResult, error) {
	if e := checkJobID(routeCancel, jobID); e != nil {
		return CancelResult{}, e
	}
	resp, err := a.c.Do(ctx, modclient.Request{
		Module: modclient.M1, Route: routeCancel, Method: http.MethodPost, Path: "/v1/jobs/" + url.PathEscape(jobID) + "/cancel", Idempotent: true,
	})
	if err != nil {
		return CancelResult{}, err
	}
	var w wireCancel
	if err := modclient.Decode(resp, &w, cancelShape); err != nil {
		return CancelResult{}, err
	}
	if !knownCancelOutcome(w.CancelOutcome) {
		return CancelResult{}, contractErr(routeCancel, resp, "cancel_outcome", "the cancel outcome is not one of cancelled, already_cancelled or already_complete")
	}
	st, err := judgeStatus(routeCancel, resp, jobID, w.wireStatus)
	if err != nil {
		return CancelResult{}, err
	}
	return CancelResult{Outcome: w.CancelOutcome, Status: st}, nil
}
