package api

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/coldmail-platform/platform/internal/adapters/m3"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/modclient"
	"github.com/coldmail-platform/platform/internal/suppress"
)

// THE CODES OF PHASE 6 A4 (module 3's accounts and the suppression list). They are the API's words for what the adapter and the stores refuse; none is module text, and each has its message here, its row
// in Docs/jobs-api.md (a test reads the document both ways) and its wording in the copy catalogue (gate A8). classifyExtra is called by classify, so one function still decides what a client is told.
const (
	// codeAccountDuplicate: the user already has an account with this SMTP host and username (module 3's create, adapter m3.ErrAccountDuplicate).
	codeAccountDuplicate = "account_duplicate"
	// codeAccountUnavailable: module 3 refuses the action and says no more: its test button and a bulk add answer so for a suspended user (m3.ErrActionUnavailable), and its create answers so for the account
	// limit and for a suspended user alike (sendapi.ErrConflictUnexplained). The platform does not guess which.
	codeAccountUnavailable = "account_action_unavailable"
	// codeHostForbidden: the SMTP host (or an address it resolves to) is one module 3 will not connect to (m3.ErrHostForbidden).
	codeHostForbidden = "host_forbidden"
	// codeFromGateAckRequired: the From address got a warning from module 3's From-gate and the account was not added until the user acknowledges it (a 422 naming acknowledge_alignment_warning).
	codeFromGateAckRequired = "from_gate_ack_required"
	// codeRateLimited: module 3 answered 429 (the test button and the account writes are limited per account and per user over a rolling day). The answer carries Retry-After.
	codeRateLimited = "rate_limited"
	// codeOutcomeUnknown: a write to module 3 (add, bulk add, test, remove) was sent and no usable answer came back, so whether it took effect is not known (m3.MaybeApplied): look at the list before trying again.
	codeOutcomeUnknown = "account_outcome_unknown"
)

func init() {
	for code, msg := range map[string]string{
		codeAccountDuplicate:    "You already have an account with this SMTP host and username.",
		codeAccountUnavailable:  "This account action is not available right now.",
		codeHostForbidden:       "This SMTP host cannot be used.",
		codeFromGateAckRequired: "The From address has a warning that must be acknowledged before the account is added.",
		codeRateLimited:         "Too many attempts; try again later.",
		codeOutcomeUnknown:      "The outcome could not be confirmed; check the list of accounts before trying again.",
	} {
		messages[code] = msg
	}
}

// defaultRateRetryAfter is the Retry-After sent with a rate_limited answer when module 3 gave none (it sends none today): its limits are counted over a rolling day, so a short promise would be a guess.
const defaultRateRetryAfter = time.Hour

// m3Failure marks an error that came from a call to module 3 made for one of the account routes, and says whether the call was a WRITE (add, bulk add, test, remove) or a read. Only the errors carrying the mark are
// mapped by classifyM3: a module error that reaches classify any other way (the job store wraps what module 3's pre-check says) keeps the mapping it always had. Only for a write does "sent, and no usable
// answer" mean the outcome is open: a read that timed out changed nothing.
type m3Failure struct {
	err   error
	write bool
}

func (f *m3Failure) Error() string { return f.err.Error() }
func (f *m3Failure) Unwrap() error { return f.err }

// m3Err marks err (not nil) as the error of a call to module 3 for an account route.
func m3Err(err error, write bool) error {
	if err == nil {
		return nil
	}
	return &m3Failure{err: err, write: write}
}

// classifyExtra maps what the suppression store and module 3's adapter return. ok is false for an error that is neither: classify then goes on with the store's own.
func classifyExtra(err error) (problem, bool) {
	var inv *suppress.InvalidAddressError
	switch {
	case errors.As(err, &inv):
		// the platform's address rule refused it: the word of the closed vocabulary, never the address
		return problem{status: http.StatusBadRequest, code: codeValidationFailed, expected: true,
			details: []any{"reason", "the address is not valid", "address_reason", suppress.ReasonOf(err)}}, true
	case errors.Is(err, suppress.ErrCannotStore):
		return problem{status: http.StatusBadRequest, code: codeValidationFailed, expected: true,
			details: []any{"reason", "the address cannot be stored", "address_reason", suppress.ReasonCannotStore}}, true
	case errors.Is(err, suppress.ErrBadQuery):
		return problem{status: http.StatusBadRequest, code: codeValidationFailed, expected: true, details: []any{"reason", "the search text is not usable"}}, true
	case errors.Is(err, suppress.ErrBadCursor):
		return problem{status: http.StatusBadRequest, code: codeValidationFailed, expected: true, details: []any{"reason", "the cursor is not one this API gave"}}, true
	}
	return classifyM3(err)
}

// m3Fields are the request fields of an account a refusal of module 3 may name, and m3Issues the words it says about one (api-contract.md: details[].issue). Anything outside them is dropped.
var (
	m3Fields = map[string]bool{"smtp_host": true, "smtp_port": true, "smtp_username": true, "smtp_secret": true, "from_address": true, "from_name": true, "daily_limit": true}
	m3Issues = map[string]bool{"required": true, "out_of_range": true, "malformed": true, "not_found": true, "conflict": true, "forbidden_value": true}
)

// hasDetail: the module's refusal names this field with this issue.
func hasDetail(e *modclient.Error, field, issue string) bool {
	for _, d := range e.Details {
		if d.Field == field && d.Issue == issue {
			return true
		}
	}
	return false
}

// classifyM3 maps module 3's adapter errors to the closed set above and to the existing codes (not_found, validation_failed, module_unavailable, internal_error). Nothing of the module's text is carried: a
// field is named only when it is a field of an account, an issue only when it is one of the module's six words.
func classifyM3(err error) (problem, bool) {
	var mf *m3Failure
	if !errors.As(err, &mf) {
		return problem{}, false
	}
	write := mf.write
	var me *modclient.Error
	switch {
	case errors.Is(err, m3.ErrAccountDuplicate):
		return problem{status: http.StatusConflict, code: codeAccountDuplicate, expected: true}, true
	case errors.Is(err, m3.ErrHostForbidden):
		return problem{status: http.StatusUnprocessableEntity, code: codeHostForbidden, expected: true}, true
	case errors.Is(err, m3.ErrActionUnavailable), errors.Is(err, sendapi.ErrConflictUnexplained):
		return problem{status: http.StatusConflict, code: codeAccountUnavailable, expected: true}, true
	case !errors.As(err, &me):
		return problem{}, false
	}
	switch me.Kind {
	case modclient.Refused:
		switch {
		case me.Status == http.StatusUnprocessableEntity && hasDetail(me, "acknowledge_alignment_warning", "required"):
			return problem{status: http.StatusUnprocessableEntity, code: codeFromGateAckRequired, expected: true, details: []any{"from_gate", string(m3.AlignmentWarned)}}, true
		case me.Status == http.StatusBadRequest && me.Code == "validation_failed":
			var fields []string
			for _, d := range me.Details {
				if m3Fields[d.Field] && m3Issues[d.Issue] {
					fields = append(fields, d.Field+":"+d.Issue)
				}
			}
			if fields == nil {
				fields = []string{}
			}
			return problem{status: http.StatusBadRequest, code: codeValidationFailed, expected: true, details: []any{"reason", "the account is not valid", "fields", fields}}, true
		}
		return problem{}, false // a refusal the API does not know (a revoked service token, an unprocessable it cannot name): the server's fault, a 500 whose cause is logged
	case modclient.NotFound:
		return problem{status: http.StatusNotFound, code: codeNotFound, expected: true}, true // the account is not this user's, is gone or never was: the module answers the three alike, and so does the API
	case modclient.RateLimited:
		after := me.RetryAfter
		if after <= 0 {
			after = defaultRateRetryAfter
		}
		return problem{status: http.StatusTooManyRequests, code: codeRateLimited, expected: true, retryAfter: strconv.Itoa(int((after + time.Second - 1) / time.Second))}, true
	case modclient.Paused, modclient.Unavailable, modclient.Timeout, modclient.Canceled, modclient.Internal, modclient.Contract:
		if write && m3.MaybeApplied(err) {
			return problem{status: http.StatusServiceUnavailable, code: codeOutcomeUnknown, expected: me.Kind != modclient.Internal && me.Kind != modclient.Contract}, true
		}
		// the module could not be asked, or answered a read it should not have: not the user's fault. A module that is up and broken (Internal, Contract) is logged as the fault it is.
		return problem{status: http.StatusServiceUnavailable, code: codeModuleUnavailable, expected: me.Kind != modclient.Internal && me.Kind != modclient.Contract}, true
	}
	return problem{}, false
}
