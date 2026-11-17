package m1

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/coldmail-platform/platform/internal/modclient"
)

// SubmitCounts is the 202's counts over INPUT ROWS: Total = Enqueued + Duplicate + Malformed. Enqueued counts distinct addresses; Duplicate the rows that repeat an earlier row's
// address inside this job.
type SubmitCounts struct {
	Total     int
	Enqueued  int
	Duplicate int
	Malformed int
}

// ErrOrphanPossible is what errors.Is(err, ErrOrphanPossible) finds in the error of a bulk submission that was not confirmed: the request may have reached module 1 and a job may
// exist that the platform has no id for. Module 1 cannot be asked ("which jobs did I submit?": there is no listing and no lookup by caller_ref), so the caller must record the
// intent BEFORE calling Submit and treat this error as "a job of this batch may exist; it will be charged only if its results are ever read".
var ErrOrphanPossible = errors.New("m1: an orphan job is possible: the bulk submission was not confirmed")

// OrphanError is the error of an unconfirmed submission: a request that was sent and got no usable answer (a timeout, a connection reset, a 5xx, an answer that is not the 202 of the
// contract). It wraps the *modclient.Error (modclient.KindOf and errors.As see through it) and is ErrOrphanPossible.
type OrphanError struct {
	Err *modclient.Error
}

func (e *OrphanError) Error() string { return ErrOrphanPossible.Error() + ": " + e.Err.Error() }

func (e *OrphanError) Unwrap() error { return e.Err }

// Is makes errors.Is(err, ErrOrphanPossible) true.
func (e *OrphanError) Is(target error) bool { return target == ErrOrphanPossible }

// MismatchError is a Contract error about a CONFIRMED submission: module 1 accepted the job (JobID is its id, a validated uuid) but its counts are not the ones the platform's plan
// predicted, or do not add up. The job exists; a caller that gets this error has the id it needs to account for it.
type MismatchError struct {
	JobID string
	Err   *modclient.Error
}

func (e *MismatchError) Error() string {
	return "m1: module 1 accepted job " + e.JobID + " but its answer is not what the plan predicted: " + e.Err.Error()
}

func (e *MismatchError) Unwrap() error { return e.Err }

// orphanIfUnconfirmed wraps err in an OrphanError when the request was sent and the module did not say, in terms the contract defines, that it created nothing.
//
// MODULE-FORCED(M1-F2): module 1's bulk submission is not idempotent (no key, no lookup by caller_ref): a retry after a lost answer creates a second full job that is verified,
// stored and paid for by the platform's vendor and node budget, so the adapter never retries it and reports every unconfirmed outcome as "an orphan job is possible".
//
// Not unconfirmed: a request that never left (Sent false: connection refused, name not found, TLS refused), and an answer in which module 1 refused (Refused, RateLimited,
// NotFound) or said it is paused (503 `paused`, which its contract defines as "retry unchanged later"). Unconfirmed: a timeout, a reset or truncated connection, any other 503, a
// 502, 504 or 500, the caller's context ending after the send, and a 2xx that is not the documented 202.
func orphanIfUnconfirmed(err error) error {
	e := asModclient(err)
	if e == nil || !e.Sent {
		return err
	}
	switch e.Kind {
	case modclient.Timeout, modclient.Unavailable, modclient.Internal, modclient.Contract, modclient.Canceled:
		return &OrphanError{Err: e}
	}
	return err
}

type submitBody struct {
	Items []Item `json:"items"`
}

// Submit sends the plan's rows as ONE bulk job and returns module 1's job id and the counts of its 202, which must be exactly the plan's Expected counts: a difference is a
// *MismatchError (Contract kind, naming the counts key, carrying the job id) and not a shrug. The request is never retried by anything (M1-F2). A plan with no rows to send is an
// Invalid error (a batch of malformed rows has nothing to submit: settle MalformedSettleItems), and a plan that Err refused returns that error; neither makes a request.
//
// The job id is a validated uuid: module 1 answers a job route with 500 for anything else (M1-F1), so a 202 whose id is not one is a contract error.
func (a *Adapter) Submit(ctx context.Context, plan Plan) (string, SubmitCounts, error) {
	if plan.refusal != nil {
		return "", SubmitCounts{}, plan.refusal
	}
	if len(plan.Items) == 0 {
		return "", SubmitCounts{}, newErr(modclient.Invalid, routeSubmit, "the plan has no rows to send (every row is malformed, or there are none): settle the malformed rows and do not submit")
	}
	id, got, err := a.submit(ctx, plan.Items)
	if err != nil {
		return "", SubmitCounts{}, err
	}
	// MODULE-FORCED(M1-F8): module 1 reports how many rows were duplicates and how many it judged malformed only in the 202 (the status route repeats neither and the results
	// carry them per row), so the 202 is the one place the adapter can check module 1's norm/v1 against its own, and it does it here, before the job is believed.
	if key := countsDisagree(plan.Expected(), got); key != "" {
		e := contractErr(routeSubmit, nil, key, fmt.Sprintf("module 1 counted %s differently from the plan (module 1 says %d)", key, countOf(got, key)))
		e.Status = http.StatusAccepted
		return "", SubmitCounts{}, &MismatchError{JobID: id, Err: e}
	}
	return id, got, nil
}

// SubmitRaw sends exactly these rows, with none of Prepare's filtering: malformed addresses go to module 1 too, which answers them itself (invalid/invalid_syntax) and counts them.
// It is NOT the production path (Submit is: a malformed address is never sent, row P-08). It exists for the harness tests that prove module 1's own handling of such rows and its
// agreement with internal/norm, and for nothing else. It applies module 1's limits on item_ids, rows and body, never retries, and returns the 202's counts unchecked against any
// prediction (the caller has none).
func (a *Adapter) SubmitRaw(ctx context.Context, items []Item) (string, SubmitCounts, error) {
	if len(items) == 0 {
		return "", SubmitCounts{}, newErr(modclient.Invalid, routeSubmit, "there are no rows to send")
	}
	refs := make([]string, len(items))
	bodyBytes := len(`{"items":[]}`) + len(items) - 1
	for i, it := range items {
		refs[i] = it.ItemID
		bodyBytes += itemBytes(it)
	}
	if e := checkRefs(refs, "items", "item_id"); e != nil {
		return "", SubmitCounts{}, e
	}
	if e := checkLimits(len(items), bodyBytes, module1Limits); e != nil {
		return "", SubmitCounts{}, e
	}
	id, got, err := a.submit(ctx, items)
	if err != nil {
		return "", SubmitCounts{}, err
	}
	return id, got, nil
}

// submit makes the one POST and decodes its 202. On a failure the error is an *OrphanError when the outcome is unconfirmed, else the modclient error as it came.
func (a *Adapter) submit(ctx context.Context, items []Item) (string, SubmitCounts, error) {
	resp, err := a.c.Do(ctx, modclient.Request{
		Module: modclient.M1, Route: routeSubmit, Method: http.MethodPost, Path: "/v1/verify/bulk",
		Body: submitBody{Items: items}, Expect: []int{http.StatusAccepted},
		Timeout: max(a.timeout, submitTimeoutFloor), Idempotent: false,
	})
	if err != nil {
		return "", SubmitCounts{}, orphanIfUnconfirmed(err)
	}
	var w struct {
		JobID  string `json:"job_id"`
		Status string `json:"status"`
		Counts struct {
			Total     int `json:"total"`
			Enqueued  int `json:"enqueued"`
			Duplicate int `json:"duplicate"`
			Malformed int `json:"malformed"`
		} `json:"counts"`
	}
	if err := modclient.Decode(resp, &w, submitShape); err != nil {
		return "", SubmitCounts{}, orphanIfUnconfirmed(err)
	}
	switch {
	case !knownSubmitStatus(w.Status):
		return "", SubmitCounts{}, orphanIfUnconfirmed(contractErr(routeSubmit, resp, "status", "the 202 does not say accepted"))
	case !validJobID(w.JobID):
		return "", SubmitCounts{}, orphanIfUnconfirmed(contractErr(routeSubmit, resp, "job_id", "the 202 carries a job id that is not a uuid, so no job route could be called with it"))
	}
	c := SubmitCounts{Total: w.Counts.Total, Enqueued: w.Counts.Enqueued, Duplicate: w.Counts.Duplicate, Malformed: w.Counts.Malformed}
	for _, k := range []string{"total", "enqueued", "duplicate", "malformed"} {
		if countOf(c, "counts."+k) < 0 {
			return "", SubmitCounts{}, &MismatchError{JobID: w.JobID, Err: contractErr(routeSubmit, resp, "counts."+k, "a count in the 202 is negative")}
		}
	}
	if c.Total != c.Enqueued+c.Duplicate+c.Malformed {
		return "", SubmitCounts{}, &MismatchError{JobID: w.JobID, Err: contractErr(routeSubmit, resp, "counts.total", "the counts of the 202 do not add up (total is not enqueued + duplicate + malformed)")}
	}
	return w.JobID, c, nil
}

// countsDisagree names the first key of the 202's counts that differs from the plan's prediction ("" when they agree).
func countsDisagree(want, got SubmitCounts) string {
	for _, k := range []string{"counts.total", "counts.enqueued", "counts.duplicate", "counts.malformed"} {
		if countOf(want, k) != countOf(got, k) {
			return k
		}
	}
	return ""
}

func countOf(c SubmitCounts, key string) int {
	switch key {
	case "counts.total":
		return c.Total
	case "counts.enqueued":
		return c.Enqueued
	case "counts.duplicate":
		return c.Duplicate
	case "counts.malformed":
		return c.Malformed
	}
	return -1
}
