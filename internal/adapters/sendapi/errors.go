package sendapi

import (
	"errors"
	"slices"
	"strings"

	"github.com/coldmail-platform/platform/internal/modclient"
)

// The sentinels of the package. Each is carried by an error that ALSO unwraps to the modclient *Error it refines, so errors.Is(err, modclient.ErrRefused), modclient.KindOf(err) and
// errors.As(err, &*modclient.Error) keep working on it: a caller that knows nothing of these sentinels still gets the right Kind.
var (
	// ErrBackpressure: backpressure: another campaign of this user is active. Module 2's 409 on run: the user has a job of a different campaign in queued, assigned or sending (and stays so
	// while a cancelled campaign's in-flight items finish). Nothing is wrong and nothing was created: ask again later, unchanged; the job engine serialises a user's campaigns.
	ErrBackpressure = errors.New("sendapi: backpressure: another campaign of this user is active")
	// ErrConflictUnexplained: a 409 on run whose cause the module does not say: a module 3 older than Phase 3 close-out C2 (its 409 carries no details) answers so. It is NEVER what a module that
	// names its causes answers (that is a *ConflictError), and module 2 (back-pressure, ErrBackpressure) never answers it. The adapter never picks a cause for it.
	ErrConflictUnexplained = errors.New("sendapi: the module refused the run with a conflict whose cause it does not say")
	// ErrBadCursor: the module did not accept a cursor (a 400 validation_failed naming the cursor). The adapter only ever passes back the next_cursor the module issued, so this means a forged or
	// corrupted cursor. A module fault (a 500) on a request that carried a cursor is NOT this error: it stays an Internal error.
	ErrBadCursor = errors.New("sendapi: the module did not accept the page cursor")
	// The four causes of module 3's run 409 (its closed vocabulary run_conflict_cause), one sentinel each: errors.Is(err, ErrRunSuspended) is true of a *ConflictError that names it.
	// ErrRunSuspended: the user is suspended (stop: nothing is accepted until the suspension is lifted).
	ErrRunSuspended = errors.New("sendapi: the module refused the run: the user is suspended")
	// ErrRunCampaignActive: ANOTHER campaign of the user still has a job that is not final (one active campaign per user: wait).
	ErrRunCampaignActive = errors.New("sendapi: the module refused the run: another campaign of this user is active")
	// ErrRunCampaignCap: this campaign already holds the module's campaign size cap (split the campaign).
	ErrRunCampaignCap = errors.New("sendapi: the module refused the run: the campaign holds the campaign size cap")
	// ErrRunQueueCap: the user's live queue already holds the module's queue depth cap (wait for it to drain).
	ErrRunQueueCap = errors.New("sendapi: the module refused the run: the user's queue holds the queue depth cap")
)

// wrapped is an error that is a sentinel and also unwraps to the underlying error (Go 1.20 multiple unwrap).
type wrapped struct {
	sentinel error
	err      error
}

func (w *wrapped) Error() string   { return w.sentinel.Error() + " (" + w.err.Error() + ")" }
func (w *wrapped) Unwrap() []error { return []error{w.sentinel, w.err} }

func refine(sentinel, err error) error { return &wrapped{sentinel: sentinel, err: err} }

// invalid is an error raised before anything was sent (a bad argument): modclient's Invalid kind, so the one switch on Kind covers it.
func (c *Client) invalid(op, msg string) error {
	return &modclient.Error{Kind: modclient.Invalid, Module: c.p.Module, Route: c.p.route(op), Message: msg}
}

// contract is a Contract error about a response that was received and decoded but whose content the adapter refuses: the key and a sentence of ours, never a value from the response.
func (c *Client) contract(resp *modclient.Response, op, key, msg string) error {
	e := &modclient.Error{Kind: modclient.Contract, Module: c.p.Module, Route: c.p.route(op), Key: key, Message: msg, Sent: true}
	if resp != nil {
		e.Status, e.RequestID, e.Attempts = resp.Status, resp.RequestID, resp.Attempts
	}
	return e
}

// Cause is one of the closed causes of module 3's run 409 (its describe vocabulary run_conflict_cause). The module names each as the `field` and the `code` of one entry of the 409's
// details[]; the two are the same word, and modclient keeps `field`, so that is the one read.
type Cause string

const (
	// CauseSuspended: the user is suspended.
	CauseSuspended Cause = "suspended"
	// CauseCampaignActive: another campaign of the user is active.
	CauseCampaignActive Cause = "campaign_active"
	// CauseCampaignCap: this campaign holds the campaign size cap.
	CauseCampaignCap Cause = "campaign_cap"
	// CauseQueueCap: the user's live queue holds the queue depth cap.
	CauseQueueCap Cause = "queue_cap"
)

// runCauses is the closed set in the module's own order (the order of details[] and of the quote's reasons): the vocabulary run_conflict_cause as the adapter handles it.
var runCauses = []string{string(CauseSuspended), string(CauseCampaignActive), string(CauseCampaignCap), string(CauseQueueCap)}

// RunCauses is the closed set of causes the adapter handles, in the module's order. The slice is a copy.
func RunCauses() []Cause {
	out := make([]Cause, len(runCauses))
	for i, c := range runCauses {
		out[i] = Cause(c)
	}
	return out
}

// Valid is true for exactly the four causes of the closed set.
func (c Cause) Valid() bool { return slices.Contains(runCauses, string(c)) }

// Sentinel is the sentinel error of a cause (ErrRunSuspended, ErrRunCampaignActive, ErrRunCampaignCap, ErrRunQueueCap), nil for a word outside the closed set.
func (c Cause) Sentinel() error {
	switch c {
	case CauseSuspended:
		return ErrRunSuspended
	case CauseCampaignActive:
		return ErrRunCampaignActive
	case CauseCampaignCap:
		return ErrRunCampaignCap
	case CauseQueueCap:
		return ErrRunQueueCap
	}
	return nil
}

// ConflictError is module 3's 409 on run, explained: the causes the module named, each once, in the module's order. It unwraps to one sentinel per cause AND to the modclient Refused
// error it refines (status 409, code conflict), so errors.Is(err, ErrRunQueueCap), errors.Is(err, modclient.ErrRefused) and modclient.KindOf(err) all hold, and it never names a cause
// the module did not. Use CausesOf to read the causes without knowing the type.
type ConflictError struct {
	// Causes are the causes the module named, in the order it named them (its fixed order: suspended, campaign_active, campaign_cap, queue_cap). There is at least one.
	Causes []Cause
	err    error
}

// Error lists the causes (words of the closed set) and nothing from the response.
func (e *ConflictError) Error() string {
	words := make([]string, len(e.Causes))
	for i, c := range e.Causes {
		words[i] = string(c)
	}
	return "sendapi: the module refused the run with a conflict: " + strings.Join(words, ", ") + " (" + e.err.Error() + ")"
}

// Unwrap is the sentinel of each cause and then the modclient error.
func (e *ConflictError) Unwrap() []error {
	out := make([]error, 0, len(e.Causes)+1)
	for _, c := range e.Causes {
		out = append(out, c.Sentinel())
	}
	return append(out, e.err)
}

// Has reports whether the module named the cause.
func (e *ConflictError) Has(c Cause) bool { return slices.Contains(e.Causes, c) }

// CausesOf are the causes a run's error names: the causes of a *ConflictError in the chain, in the module's order, and nil for any other error (a 409 with no named cause, an error of
// another kind, nil). The slice is a copy.
func CausesOf(err error) []Cause {
	var ce *ConflictError
	if !errors.As(err, &ce) {
		return nil
	}
	return slices.Clone(ce.Causes)
}

// conflict explains a 409 on run for a profile whose module names its causes. A 409 with no details is a module that does not say (ErrConflictUnexplained, never a guess); an entry that
// is not a cause of the closed set, one that names a cause twice, or a 409 whose entries are not all `conflict` is a Contract error naming error.details (the module spoke outside the
// vocabulary it advertises: a person looks at it before anything is decided from it). The error never carries a value of the response.
func (c *Client) conflict(e *modclient.Error, err error) error {
	if len(e.Details) == 0 {
		return refine(ErrConflictUnexplained, err)
	}
	var causes []Cause
	for _, d := range e.Details {
		cause := Cause(d.Field)
		switch {
		case d.Issue != "conflict" || !cause.Valid():
			return c.badConflict(e, "error.details[].field", "an entry of the 409's details is not one of the closed causes of a run refusal (a new cause is never mapped by guesswork)")
		case slices.Contains(causes, cause):
			return c.badConflict(e, "error.details[].field", "the 409's details name one cause twice")
		}
		causes = append(causes, cause)
	}
	return &ConflictError{Causes: causes, err: err}
}

func (c *Client) badConflict(e *modclient.Error, key, msg string) error {
	return &modclient.Error{Kind: modclient.Contract, Module: c.p.Module, Route: c.p.route(opRun), Status: e.Status, RequestID: e.RequestID, Key: key, Message: msg, Sent: true, Attempts: e.Attempts}
}

// mapErr refines the errors of one call, never changes a Kind except to Contract for an answer outside a closed vocabulary, and never turns an error into a value.
//
// The refinements exist because the modules' bodies do not say enough or say it in details:
//   - run's 409: see Conflict (module 2: back-pressure, no details; module 3: the causes it names).
//   - a cursor the module did not accept: a 400 naming the cursor (both modules answer so for a cursor that is not base64 and for one that decodes but is not a timestamp and an id). A 500
//     on a request that carried a cursor stays a plain Internal error: since Phase 4 A1 no module answers a forged cursor with a 500, so a 500 cannot be claimed as a bad cursor.
func (c *Client) mapErr(op string, err error, usedCursor bool) error {
	var e *modclient.Error
	if !errors.As(err, &e) {
		return err
	}
	switch {
	case op == opRun && e.Kind == modclient.Refused && e.Status == 409 && e.Code == "conflict":
		// MODULE-FORCED(SA-F1): module 2 answers run's 409 with one body (code conflict, no details; owner ruling OD-5) and a module 3 older than the close-out answers it the same way, so the adapter can only call the first back-pressure and the second unexplained, never name a cause the body does not.
		if c.p.RunConflict == ConflictBackpressure {
			return refine(ErrBackpressure, err)
		}
		return c.conflict(e, err)
	case op == opReportCamp && usedCursor && e.Kind == modclient.Refused && hasDetail(e, "cursor"):
		return refine(ErrBadCursor, err)
	}
	return err
}

func hasDetail(e *modclient.Error, field string) bool {
	for _, d := range e.Details {
		if d.Field == field {
			return true
		}
	}
	return false
}

// NotFoundField names the resource a 404 was about: the `field` of the module's first detail ("job_item_id", "job_id"), and ok = false when err is not a NotFound error or the module
// named nothing. It is what the platform logs; it is not a reason to retry or to give up (see ReportJob).
func NotFoundField(err error) (field string, ok bool) {
	var e *modclient.Error
	if !errors.As(err, &e) || e.Kind != modclient.NotFound {
		return "", false
	}
	// MODULE-FORCED(SA-F3): a 404 on a report is the same answer for a job that was never run, another tenant's job and a typo, so the adapter returns the module's NotFound untouched and only names the resource; the caller decides whether that is "not yet" (an unsettled item) or a platform bug.
	for _, d := range e.Details {
		if d.Issue == "not_found" {
			return d.Field, true
		}
	}
	return "", false
}
