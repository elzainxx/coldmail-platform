package sendapi

import (
	"errors"

	"github.com/coldmail-platform/platform/internal/modclient"
)

// The sentinels of the package. Each is carried by an error that ALSO unwraps to the modclient *Error it refines, so errors.Is(err, modclient.ErrRefused), modclient.KindOf(err) and
// errors.As(err, &*modclient.Error) keep working on it: a caller that knows nothing of these sentinels still gets the right Kind.
var (
	// ErrBackpressure: backpressure: another campaign of this user is active. Module 2's 409 on run: the user has a job of a different campaign in queued, assigned or sending (and stays so
	// while a cancelled campaign's in-flight items finish). Nothing is wrong and nothing was created: ask again later, unchanged; the job engine serialises a user's campaigns.
	ErrBackpressure = errors.New("sendapi: backpressure: another campaign of this user is active")
	// ErrConflictUnexplained: module 3's 409 on run. The module answers one body for four causes (suspended, another campaign active, the campaign cap, the queue cap) and the adapter cannot
	// tell them apart: the platform checks them from its own state before it submits and treats this as "refused, cause unknown".
	ErrConflictUnexplained = errors.New("sendapi: the module refused the run with a conflict whose cause it does not say")
	// ErrBadCursor: the module did not accept a cursor. The adapter only ever passes back the next_cursor the module issued, so this means a forged or corrupted cursor, or a module fault on a
	// request that carried one.
	ErrBadCursor = errors.New("sendapi: the module did not accept the page cursor")
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

// mapErr refines the errors of one call, never changes a Kind and never turns an error into a value.
//
// The three refinements exist because the modules' bodies do not say enough:
//   - run's 409: see Conflict.
//   - a cursor the module did not accept (a 400 naming the cursor, or a 500: a syntactically valid cursor that is not a timestamp and an id reaches a cast in the module's SQL).
func (c *Client) mapErr(op string, err error, usedCursor bool) error {
	var e *modclient.Error
	if !errors.As(err, &e) {
		return err
	}
	switch {
	case op == opRun && e.Kind == modclient.Refused && e.Status == 409 && e.Code == "conflict":
		// MODULE-FORCED(SA-F1): run answers 409 with the same body (code conflict, no details) for every cause, so the adapter infers the cause from the route and the profile, never from the body.
		if c.p.RunConflict == ConflictBackpressure {
			return refine(ErrBackpressure, err)
		}
		return refine(ErrConflictUnexplained, err)
	case op == opReportCamp && usedCursor && e.Kind == modclient.Refused && hasDetail(e, "cursor"):
		return refine(ErrBadCursor, err)
	case op == opReportCamp && usedCursor && e.Kind == modclient.Internal && e.Status == 500:
		// MODULE-FORCED(SA-F2): a cursor that decodes but is not a timestamp and an id is a 500 from the module (not the 400 its documentation promises) and cannot be told from a module fault, so a 500 on a request that carried a cursor is reported as a possible bad cursor.
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
