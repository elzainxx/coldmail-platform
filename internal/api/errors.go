package api

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/coldmail-platform/platform/internal/httpapi"
	"github.com/coldmail-platform/platform/internal/jobs"
	"github.com/coldmail-platform/platform/internal/money"
)

// The codes of the API's error envelope: user vocabulary, additive-only (never renamed or repurposed), documented in Docs/jobs-api.md (a test holds the document to this list).
const (
	codeUnauthenticated   = "unauthenticated"
	codeNotFound          = "not_found"
	codeValidationFailed  = "validation_failed"
	codeBadRequest        = "bad_request"
	codePayloadTooLarge   = "payload_too_large"
	codeUnsupportedMedia  = "unsupported_media_type"
	codeTooManyRows       = "too_many_rows"
	codeNothingToDo       = "nothing_to_do"
	codePreviewStale      = "preview_stale"
	codeJobSlotTaken      = "job_slot_taken"
	codeIdemReused        = "idempotency_key_reused"
	codeNotPreviewed      = "not_previewed"
	codeInsufficient      = "insufficient_credits"
	codeNoEntitlement     = "no_entitlement"
	codeSuspended         = "suspended"
	codeInfeasible        = "infeasible"
	codeModuleUnavailable = "module_unavailable"
	codeNotAllowed        = "not_allowed"
	codeBusy              = "busy"
	codeInternal          = "internal_error"
)

// messages are the only texts a client ever sees for a code. Nothing a caller sent is reflected and nothing internal (a driver error, a constraint, SQL) is serialised.
var messages = map[string]string{
	codeUnauthenticated:   "Authentication is required.",
	codeNotFound:          "The requested resource was not found.",
	codeValidationFailed:  "The request is not valid.",
	codeBadRequest:        "The request could not be read.",
	codePayloadTooLarge:   "The request body is too large.",
	codeUnsupportedMedia:  "The request body has a content type this route does not accept.",
	codeTooManyRows:       "The list has more rows than a job may hold.",
	codeNothingToDo:       "No row of the list would reach the module.",
	codePreviewStale:      "The preview is out of date: preview the job again and confirm what the new preview says.",
	codeJobSlotTaken:      "You already have an active job on this module.",
	codeIdemReused:        "This Idempotency-Key already confirmed another job.",
	codeNotPreviewed:      "The job must be previewed before it can be confirmed.",
	codeInsufficient:      "There are not enough credits for this list.",
	codeNoEntitlement:     "This module is not enabled for your account.",
	codeSuspended:         "Your account is suspended on this module.",
	codeInfeasible:        "The module says this campaign cannot run.",
	codeModuleUnavailable: "The module cannot be reached right now; try again later.",
	codeNotAllowed:        "The job's state does not allow this.",
	codeBusy:              "The service is busy with another operation on the same credits; try again.",
	codeInternal:          "An unexpected error occurred.",
}

// problem is what an error becomes on the wire.
type problem struct {
	status  int
	code    string
	details []any // key/value pairs for httpapi.WriteError; nil when the code has nothing to say
	// expected is false for a problem that is the server's fault (it is logged as an error with the cause); true for a refusal the client caused.
	expected bool
}

// classify is THE mapping from what the store (and its dependencies) return to what the client is told: the one function, covered by a table test of every sentinel. An error it does not
// know is a 500 `internal_error` with no detail: the cause is the log's.
func classify(err error) problem {
	var (
		bad  *jobs.BadRequestError
		many *jobs.TooManyRowsError
		na   *jobs.NotAllowedError
		cred jobs.ErrInsufficientCredits
		inf  jobs.ErrInfeasible
		big  *http.MaxBytesError
	)
	switch {
	case errors.Is(err, jobs.ErrNotFound):
		return problem{status: http.StatusNotFound, code: codeNotFound, expected: true}
	case errors.As(err, &bad):
		return problem{status: http.StatusBadRequest, code: codeValidationFailed, details: []any{"reason", safeReason(bad.Reason)}, expected: true}
	case errors.Is(err, jobs.ErrBadRequest):
		return problem{status: http.StatusBadRequest, code: codeValidationFailed, details: []any{"reason", "the request is not valid"}, expected: true}
	case errors.As(err, &big):
		return problem{status: http.StatusRequestEntityTooLarge, code: codePayloadTooLarge, details: []any{"limit_bytes", big.Limit}, expected: true}
	case errors.As(err, &many):
		return problem{status: http.StatusRequestEntityTooLarge, code: codeTooManyRows, details: []any{"limit", many.Limit}, expected: true}
	case errors.Is(err, jobs.ErrTooManyRows):
		return problem{status: http.StatusRequestEntityTooLarge, code: codeTooManyRows, expected: true}
	case errors.Is(err, jobs.ErrNothingToDo):
		return problem{status: http.StatusUnprocessableEntity, code: codeNothingToDo, expected: true}
	case errors.Is(err, jobs.ErrStalePreview):
		return problem{status: http.StatusConflict, code: codePreviewStale, expected: true}
	case errors.Is(err, jobs.ErrSlotTaken):
		return problem{status: http.StatusConflict, code: codeJobSlotTaken, expected: true}
	case errors.Is(err, jobs.ErrConfirmKeyReused):
		return problem{status: http.StatusConflict, code: codeIdemReused, expected: true}
	case errors.Is(err, jobs.ErrNotPreviewed):
		return problem{status: http.StatusConflict, code: codeNotPreviewed, expected: true}
	case errors.As(err, &cred):
		return problem{status: http.StatusPaymentRequired, code: codeInsufficient, details: []any{"available", cred.Available, "needed", cred.Needed, "shortfall", cred.Shortfall}, expected: true}
	case errors.Is(err, jobs.ErrNoEntitlement):
		return problem{status: http.StatusForbidden, code: codeNoEntitlement, expected: true}
	case errors.Is(err, jobs.ErrSuspended):
		return problem{status: http.StatusForbidden, code: codeSuspended, expected: true}
	case errors.As(err, &inf):
		return problem{status: http.StatusConflict, code: codeInfeasible, details: []any{"causes", safeCauses(inf.Causes)}, expected: true}
	case errors.Is(err, jobs.ErrModuleUnavailable):
		return problem{status: http.StatusServiceUnavailable, code: codeModuleUnavailable, expected: true}
	case errors.As(err, &na):
		return problem{status: http.StatusConflict, code: codeNotAllowed, details: []any{"reason", notAllowedWord(na.Reason)}, expected: true}
	case errors.Is(err, jobs.ErrNotAllowed):
		return problem{status: http.StatusConflict, code: codeNotAllowed, details: []any{"reason", "not_allowed"}, expected: true}
	case errors.Is(err, money.ErrLockTimeout):
		return problem{status: http.StatusServiceUnavailable, code: codeBusy, expected: true}
	}
	return problem{status: http.StatusInternalServerError, code: codeInternal}
}

// notAllowedWords are the reasons of the store's own that are already words a user can be told; every other reason (a constraint's name) is translated or becomes "not_allowed".
var notAllowedWords = map[string]string{
	// the store's own words (internal/jobs moves.go and preview.go)
	"not_a_draft":                           "not_a_draft",
	"already_a_draft":                       "already_a_draft",
	"not_confirmed":                         "not_confirmed",
	"not_paused":                            "not_paused",
	"start_deadline_passed":                 "start_deadline_passed",
	"submitted_job_ends_through_the_engine": "submitted_job_ends_through_the_engine",
	"job_status_has_no_preview":             "job_status_has_no_preview",
	// the database's constraint names, in a user's words
	"job_module_cannot_pause":  "pause_not_supported",
	"job_module_cannot_cancel": "cancel_not_supported",
}

// notAllowedWord is the reason a client is told for a refusal of the job's lifecycle. A constraint's name is never sent: it is translated when it has a word (a module that cannot pause or
// cancel a running job) and is "not_allowed" otherwise.
func notAllowedWord(reason string) string {
	if w, ok := notAllowedWords[reason]; ok {
		return w
	}
	return "not_allowed"
}

// addressLike finds something that could be an address in a reason or a cause.
var addressLike = regexp.MustCompile(`\S+@\S+`)

// safeReason is the store's reason for refusing a request, as a client may be told it. The store never quotes an address; this is the second wall: a reason that holds an '@' is replaced
// whole, and a long one is cut.
func safeReason(reason string) string {
	if strings.Contains(reason, "@") {
		return "the request is not valid"
	}
	if len(reason) > 300 {
		reason = reason[:300]
	}
	return reason
}

// safeCauses are the words module 3 names for a campaign that cannot run: at most twenty, each at most 120 bytes, and none that holds an '@' (they are words, never an address).
func safeCauses(causes []string) []string {
	out := make([]string, 0, len(causes))
	for _, c := range causes {
		if len(out) == 20 {
			break
		}
		if strings.Contains(c, "@") {
			continue
		}
		if len(c) > 120 {
			c = c[:120]
		}
		out = append(out, c)
	}
	return out
}

// fail answers the request with the problem err is, and logs the cause of a server fault under the request id.
func (a *API) fail(w http.ResponseWriter, r *http.Request, err error) {
	p := classify(err)
	if !p.expected {
		a.logFault(r.Context(), r, err)
	}
	if p.code == codeBusy {
		w.Header().Set("Retry-After", "1")
	}
	httpapi.WriteError(w, r, p.status, p.code, messages[p.code], p.details...)
}

// refuseRequest answers a request that is wrong in a way the API itself found (a body, a header or a parameter), with a code and the reason as a word or a short sentence of the API's own.
func (a *API) refuseRequest(w http.ResponseWriter, r *http.Request, status int, code, reason string) {
	httpapi.WriteError(w, r, status, code, messages[code], "reason", reason)
}

// logFault logs why a request became a 500: the error's text with anything address-shaped taken out, and the SQLSTATE and constraint name of a database error. Never the request, a header, a
// body, or the token.
func (a *API) logFault(ctx context.Context, r *http.Request, err error) {
	attrs := []any{"error", addressLike.ReplaceAllString(err.Error(), "[address]"), "method", r.Method, "path", r.URL.Path}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		attrs = append(attrs, "sqlstate", pe.Code, "constraint", pe.ConstraintName)
	}
	if ctx.Err() != nil {
		a.log.InfoContext(ctx, "request ended before the work did", attrs...)
		return
	}
	a.log.ErrorContext(ctx, "request failed", attrs...)
}
