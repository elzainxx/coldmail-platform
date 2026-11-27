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

// SubmitResult is what a confirmed bulk submission returns: module 1's job id (a validated uuid), the counts of its 202, and whether the module said the answer is a REPLAY.
type SubmitResult struct {
	// JobID is module 1's id of the job.
	JobID string
	// Counts are the 202's counts (for Submit, exactly the plan's Expected counts).
	Counts SubmitCounts
	// Replayed is true when module 1 answered with `Idempotent-Replayed: true`: a job already existed for this caller, this key and this request, and no second job was made. It is
	// how a caller that sent the same key again after an unconfirmed answer learns that the first request had in fact created the job. A first answer is false.
	Replayed bool
}

// ErrIdempotencyKeyReused is what errors.Is(err, ErrIdempotencyKeyReused) finds in the error of a submission whose Idempotency-Key was already used, by this caller, for a DIFFERENT
// request: module 1 answered 409 `idempotency_key_reused` and created nothing (the other job's id is not disclosed, and the key still names the original job). The error is a Refused
// *modclient.Error carrying the module's code. A repeat of the SAME request is no error: it is a replay.
var ErrIdempotencyKeyReused = errors.New("m1: the idempotency key was already used for a different request: nothing was created")

// IdempotencyKeyPattern is module 1's rule for the key: one header value of 1 to 128 characters of [A-Za-z0-9._:-] (published by its describe as idempotency.key_pattern).
const IdempotencyKeyPattern = "^[A-Za-z0-9._:-]{1,128}$"

// ValidIdempotencyKey says whether key is one module 1 takes (IdempotencyKeyPattern). Submit and SubmitRaw refuse any other before a request, so a bad key is never sent to be
// answered with a 400.
func ValidIdempotencyKey(key string) bool {
	if len(key) < 1 || len(key) > 128 {
		return false
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == ':' || c == '-') {
			return false
		}
	}
	return true
}

// checkKey refuses a key module 1 would refuse, before any request. The message never quotes the key (it may be derived from something an operator should not see in a log).
func checkKey(key string) *modclient.Error {
	if !ValidIdempotencyKey(key) {
		return newErr(modclient.Invalid, routeSubmit, "the idempotency key must be 1 to 128 characters of letters, digits, '.', '_', ':' and '-' (module 1 refuses any other with a 400); every bulk submission carries one")
	}
	return nil
}

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

// unconfirmedNote is added to the message of an error whose request was sent and got no usable answer.
const unconfirmedNote = "; the submission is not confirmed, and the same Idempotency-Key may be sent again with the same request (a repeat returns the job if it was created and makes one if it was not)"

// unconfirmed says, in the message of a submission error, that the outcome is not known and the same key may be sent again. It changes nothing else: the Kind, the status, the code and
// the cause stay as modclient made them, so Timeout and Unavailable (and the other classes of a request that was sent) are still what the caller branches on. An error of a request
// that never left (Sent false: connection refused, name not found, TLS refused), and an answer in which module 1 refused (Refused, RateLimited, NotFound) or said it is paused (503
// `paused`, "retry unchanged later"), are not unconfirmed and are returned as they are.
func unconfirmed(err error) error {
	e := asModclient(err)
	if e == nil || !e.Sent {
		return err
	}
	switch e.Kind {
	case modclient.Timeout, modclient.Unavailable, modclient.Internal, modclient.Contract, modclient.Canceled:
		c := *e
		c.Message += unconfirmedNote
		return &c
	}
	return err
}

// reusedKey turns module 1's 409 `idempotency_key_reused` into an error that errors.Is(err, ErrIdempotencyKeyReused) finds. It stays the Refused *modclient.Error modclient made (status,
// code, request id and details intact); only its cause and its message are set. Any other error, and a 409 with another code, is returned as it is.
func reusedKey(err error) error {
	e := asModclient(err)
	if e == nil || e.Kind != modclient.Refused || e.Status != http.StatusConflict || e.Code != "idempotency_key_reused" {
		return err
	}
	c := *e
	c.Cause = ErrIdempotencyKeyReused
	c.Message += ": this Idempotency-Key already holds a job of a different request, and nothing was created"
	return &c
}

type submitBody struct {
	Items []Item `json:"items"`
	// Policy is the module 1 policy the job is created under (Phase 5 S6, ADR-0024): empty for the default policy, which is the key being ABSENT from the body (omitempty), byte for byte what Submit has always
	// sent; the platform sets it only for an opted-out job, to the key module 1's own describe published (vendor_fallback.opt_out_policy), never to a constant.
	Policy string `json:"policy,omitempty"`
}

// MaxPolicyKeyBytes is the longest policy key the adapter sends: a name, one header-sized word.
const MaxPolicyKeyBytes = 64

// ValidPolicyKey says whether key is a policy name the adapter will send or accept from a describe: 1 to MaxPolicyKeyBytes characters of letters, digits and `. _ : -`. Module 1 serves two names and refuses
// any other with a 400; the adapter refuses a name that is not even a word before a request, and a describe that publishes one is a Contract error.
func ValidPolicyKey(key string) bool {
	if len(key) < 1 || len(key) > MaxPolicyKeyBytes {
		return false
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == ':' || c == '-') {
			return false
		}
	}
	return true
}

// Submit sends the plan's rows as ONE bulk job under key and returns module 1's job id, the counts of its 202 (which must be exactly the plan's Expected counts: a difference is a
// *MismatchError, Contract kind, naming the counts key, carrying the job id) and whether the answer was a replay. A plan with no rows to send is an Invalid error (a batch of malformed rows
// has nothing to submit: settle MalformedSettleItems), a plan that Err refused returns that error, and a key that module 1 would refuse (ValidIdempotencyKey) is an Invalid error; none
// of the three makes a request.
//
// EVERY SUBMISSION CARRIES AN IDEMPOTENCY KEY, and that is what makes the request safe to repeat: module 1 scopes the key to the authenticated caller, a repeat of the same request returns
// the original job (Replayed true, no second job), and the same key with another request is a 409 (ErrIdempotencyKeyReused, Refused, nothing created). So the POST is marked
// idempotent for modclient and is RETRIED per the dials after a failure that leaves the outcome unknown (a timeout, a reset connection, a 502/503/504), always with the same key.
// When the retries are exhausted the error is the typed Unavailable or Timeout (or Internal, Contract, Canceled) of the last attempt, with a message that says the submission is not
// confirmed and the same key may be sent again; the caller, who owns the key, sends it again later. The key must be unique to ONE logical batch (derive it from the platform's job id and
// a version, never from a clock) and must be kept for as long as the batch can be retried.
//
// The job id is a validated uuid: a 202 whose id is not one is a contract error, because no job route can be called with it.
func (a *Adapter) Submit(ctx context.Context, plan Plan, key string) (SubmitResult, error) {
	return a.SubmitUnder(ctx, plan, key, "")
}

// SubmitUnder is Submit under a named module 1 policy (Phase 5 S6: the per-user vendor opt-out). policy "" is Submit exactly: the body has no `policy` member, so the job runs under module 1's default
// policy and the request is byte for byte what it always was. A policy is the key module 1 published in its describe (vendor_fallback.opt_out_policy: read it with OptOutPolicy, never write it down): it
// goes into the body as `policy`, and the SAME Idempotency-Key with the other policy is another request to module 1 (409 idempotency_key_reused, never a replay of the other job). A key that is not a
// word (ValidPolicyKey) is an Invalid error and no request is made. A module 1 that does not serve the name refuses it (400 validation_failed, field policy): it is a Refused error and nothing was created.
func (a *Adapter) SubmitUnder(ctx context.Context, plan Plan, key, policy string) (SubmitResult, error) {
	if plan.refusal != nil {
		return SubmitResult{}, plan.refusal
	}
	if len(plan.Items) == 0 {
		return SubmitResult{}, newErr(modclient.Invalid, routeSubmit, "the plan has no rows to send (every row is malformed, or there are none): settle the malformed rows and do not submit")
	}
	if e := checkKey(key); e != nil {
		return SubmitResult{}, e
	}
	if policy != "" && !ValidPolicyKey(policy) {
		return SubmitResult{}, newErr(modclient.Invalid, routeSubmit, "the policy must be 1 to 64 characters of letters, digits, '.', '_', ':' and '-' (the key module 1's describe published); the request was not sent")
	}
	res, err := a.submit(ctx, plan.Items, key, policy)
	if err != nil {
		return SubmitResult{}, err
	}
	// The 202's counts are checked against the prediction of the plan here, before the job is believed: module 1 judges rows with the same frozen norm/v1 rule the platform does, so a
	// difference means the two implementations have drifted (a validation, not a workaround: the counts are also on the job status as totals).
	if k := countsDisagree(plan.Expected(), res.Counts); k != "" {
		e := contractErr(routeSubmit, nil, k, fmt.Sprintf("module 1 counted %s differently from the plan (module 1 says %d)", k, countOf(res.Counts, k)))
		e.Status = http.StatusAccepted
		return SubmitResult{}, &MismatchError{JobID: res.JobID, Err: e}
	}
	return res, nil
}

// SubmitRaw sends exactly these rows under key, with none of Prepare's filtering: malformed addresses go to module 1 too, which answers them itself (invalid/invalid_syntax) and counts
// them. It is NOT the production path (Submit is: a malformed address is never sent, row P-08). It exists for the harness tests that prove module 1's own handling of such rows and its
// agreement with internal/norm, and for nothing else. It applies module 1's limits on item_ids, rows and body and the same key rule and retry rule as Submit, and returns the 202's counts
// unchecked against any prediction (the caller has none).
func (a *Adapter) SubmitRaw(ctx context.Context, items []Item, key string) (SubmitResult, error) {
	if len(items) == 0 {
		return SubmitResult{}, newErr(modclient.Invalid, routeSubmit, "there are no rows to send")
	}
	refs := make([]string, len(items))
	bodyBytes := len(`{"items":[]}`) + len(items) - 1
	for i, it := range items {
		refs[i] = it.ItemID
		bodyBytes += itemBytes(it)
	}
	if e := checkRefs(refs, "items", "item_id"); e != nil {
		return SubmitResult{}, e
	}
	if e := checkLimits(len(items), bodyBytes, module1Limits); e != nil {
		return SubmitResult{}, e
	}
	if e := checkKey(key); e != nil {
		return SubmitResult{}, e
	}
	res, err := a.submit(ctx, items, key, "")
	if err != nil {
		return SubmitResult{}, err
	}
	return res, nil
}

// submit makes the POST (retried per the dials, with the same key every time) and decodes its 202. On a failure the error is the modclient error as it came, with a 409
// idempotency_key_reused made findable (ErrIdempotencyKeyReused) and an unconfirmed outcome said so in its message.
func (a *Adapter) submit(ctx context.Context, items []Item, key, policy string) (SubmitResult, error) {
	resp, err := a.c.Do(ctx, modclient.Request{
		Module: modclient.M1, Route: routeSubmit, Method: http.MethodPost, Path: "/v1/verify/bulk",
		Body: submitBody{Items: items, Policy: policy}, Expect: []int{http.StatusAccepted},
		Timeout: max(a.timeout, submitTimeoutFloor), Idempotent: true,
		Header: map[string]string{idempotencyHeader: key},
	})
	if err != nil {
		return SubmitResult{}, unconfirmed(reusedKey(err))
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
		return SubmitResult{}, unconfirmed(err)
	}
	replayed := false
	switch resp.Header.Get(replayedHeader) {
	case "":
	case "true":
		replayed = true
	default:
		return SubmitResult{}, unconfirmed(contractErr(routeSubmit, resp, replayedHeader, "the response header that marks a replay is neither absent nor true"))
	}
	switch {
	case !knownSubmitStatus(w.Status):
		return SubmitResult{}, unconfirmed(contractErr(routeSubmit, resp, "status", "the 202 does not say accepted"))
	case !validJobID(w.JobID):
		return SubmitResult{}, unconfirmed(contractErr(routeSubmit, resp, "job_id", "the 202 carries a job id that is not a uuid, so no job route could be called with it"))
	}
	c := SubmitCounts{Total: w.Counts.Total, Enqueued: w.Counts.Enqueued, Duplicate: w.Counts.Duplicate, Malformed: w.Counts.Malformed}
	for _, k := range []string{"total", "enqueued", "duplicate", "malformed"} {
		if countOf(c, "counts."+k) < 0 {
			return SubmitResult{}, &MismatchError{JobID: w.JobID, Err: contractErr(routeSubmit, resp, "counts."+k, "a count in the 202 is negative")}
		}
	}
	if c.Total != c.Enqueued+c.Duplicate+c.Malformed {
		return SubmitResult{}, &MismatchError{JobID: w.JobID, Err: contractErr(routeSubmit, resp, "counts.total", "the counts of the 202 do not add up (total is not enqueued + duplicate + malformed)")}
	}
	return SubmitResult{JobID: w.JobID, Counts: c, Replayed: replayed}, nil
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
