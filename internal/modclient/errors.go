package modclient

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// Module names the module a Client talks to. It is a label for error messages and for the one rule that differs between the modules (the shape a describe must have: module 1
// has no pricing and its vocabularies carry their values); the client holds no other module-specific knowledge.
type Module string

const (
	M1 Module = "m1" // coldmail-verify
	M2 Module = "m2" // coldmail-send-managed
	M3 Module = "m3" // coldmail-send-byo
)

func (m Module) valid() bool { return m == M1 || m == M2 || m == M3 }

// Kind is the class of a failed call: what the caller may conclude from it and what it may do next. Every error this package returns is an *Error and carries exactly one.
type Kind uint8

const (
	// Unavailable: the module could not be reached or said it is not ready to serve. Connection refused, a DNS failure, a reset or truncated connection, HTTP 502 or 504,
	// HTTP 503 with the envelope code `unavailable` (or with no envelope at all). Nothing was confirmed; asking again later may work (the client itself retries an
	// idempotent request).
	Unavailable Kind = iota + 1
	// Paused: HTTP 503 with the envelope code `paused`: an operator pulled a kill switch and the very same request will succeed later, unchanged. Wait; do not page, and
	// do not retry in a loop (the client never does).
	Paused
	// Timeout: the attempt did not finish in time: no response within the request timeout, a body that stalled, the caller's own deadline, or HTTP 408.
	Timeout
	// Internal: the module answered with a fault of its own: HTTP 500 (`internal_error`, `contract_violation`) or any other 5xx but 502, 503 and 504. The module is up and
	// broken for this request. Not retried by default: the same request would hit the same fault, and a retry of a write could repeat a side effect.
	Internal
	// Contract: the module answered something this client was not told to expect: not JSON, not an object, the wrong content type, a body over the size limit, a
	// truncated document, a malformed error envelope, a status the route does not use (a 201 for a route that answers 200, a redirect, a 405), or (through Decode) a required
	// key that is missing or of the wrong type. Never retried: the next answer would be the same, and what the module meant cannot be known.
	Contract
	// Refused: the module understood the request and said no: HTTP 400, 401, 403, 409, 410, 413 or 422. Code and Details carry the module's own words. A 401 is a
	// configuration fault (a wrong or revoked key), a 409 is often back-pressure; the adapter that knows the route decides which.
	Refused
	// NotFound: HTTP 404, with or without the module's envelope (Enveloped says which: a bare 404 can be a wrong base URL or a proxy).
	NotFound
	// RateLimited: HTTP 429. RetryAfter carries the module's Retry-After as data; the client never sleeps on it except as part of retrying an idempotent request.
	RateLimited
	// Canceled: the caller's context was canceled before the call completed. (A context whose deadline passed is Timeout.)
	Canceled
	// Invalid: the request was refused here, before anything was sent: a bad route label, method, path, timeout or body, a module the client was not built for, or a
	// bad constructor argument.
	Invalid
	// NotSupported: the module has no such operation. Nothing was sent. (No operation of the three modules is NotSupported today: module 1 gained describe in the Phase 3
	// close-out; the kind stays for an operation one module lacks.)
	NotSupported
)

// String is the lower-case word of the kind.
func (k Kind) String() string {
	switch k {
	case Unavailable:
		return "unavailable"
	case Paused:
		return "paused"
	case Timeout:
		return "timeout"
	case Internal:
		return "internal"
	case Contract:
		return "contract"
	case Refused:
		return "refused"
	case NotFound:
		return "not found"
	case RateLimited:
		return "rate limited"
	case Canceled:
		return "canceled"
	case Invalid:
		return "invalid"
	case NotSupported:
		return "not supported"
	}
	return "unknown (" + strconv.Itoa(int(k)) + ")"
}

// retryable reports whether the client itself may retry a failure of this kind (when the request is idempotent): the module did not answer, or asked to be asked later.
func (k Kind) retryable() bool { return k == Unavailable || k == Timeout || k == RateLimited }

// kindSentinel is the type of the Err* values: errors.Is(err, ErrUnavailable) is true for any *Error of that Kind, however it is wrapped.
type kindSentinel Kind

func (k kindSentinel) Error() string { return "modclient: " + Kind(k).String() }

var (
	ErrUnavailable  error = kindSentinel(Unavailable)
	ErrPaused       error = kindSentinel(Paused)
	ErrTimeout      error = kindSentinel(Timeout)
	ErrInternal     error = kindSentinel(Internal)
	ErrContract     error = kindSentinel(Contract)
	ErrRefused      error = kindSentinel(Refused)
	ErrNotFound     error = kindSentinel(NotFound)
	ErrRateLimited  error = kindSentinel(RateLimited)
	ErrCanceled     error = kindSentinel(Canceled)
	ErrInvalid      error = kindSentinel(Invalid)
	ErrNotSupported error = kindSentinel(NotSupported)
)

// Detail is one entry of the module's error.details: the field it names and a short issue word (`required`, `malformed`, `out_of_range`, `not_found`, `conflict`,
// `duplicate`). Both are the module's own fixed identifiers; anything outside a safe alphabet was dropped before it got here.
type Detail struct {
	Field string
	Issue string
}

// Error is the one error type of this package. Every failure of every method is an *Error, so a caller needs one errors.As and one switch on Kind.
//
// WHAT IS IN IT. Only facts: the module, the route label the caller gave, the HTTP status, and from the module's error envelope the code, the details and the request id,
// each restricted to a short safe alphabet and dropped if it contains the bearer secret. Message is a sentence written in this package, never text from a response: a
// module's own `message` is not copied (it is a fixed sentence per code, and the code says more), and no response body, URL, query or header value ever reaches an error
// string. Cause is reachable with errors.Is and errors.As (context.DeadlineExceeded, a *net.OpError) and prints as a fixed class word, never as the underlying text, which can
// carry addresses.
type Error struct {
	Kind   Kind
	Module Module
	// Route is the caller's stable label of the call ("m2.report"), never a URL.
	Route string
	// Status is the HTTP status, 0 when there was no response.
	Status int
	// Code is the module's error.code from its envelope ("" when there was no envelope).
	Code string
	// Message is a sentence written here: what happened, in words that carry no module text.
	Message string
	// RequestID is the module's own request id: error.request_id from the envelope, else the X-Request-Id response header if the module sent one (none of the three
	// does today). It is what an operator greps for in the module's log.
	RequestID string
	// Details is the module's error.details, in its order.
	Details []Detail
	// Key names the response key that broke the contract (Contract errors raised by Decode and the typed operations): "results[3].state", "pricing.currency".
	Key string
	// RetryAfter is the module's Retry-After as data (429 and 503 only; 0 when absent or unreadable).
	RetryAfter time.Duration
	// Enveloped is true when the response carried a well-formed module error envelope. A bare 404 or 503 from a proxy has none.
	Enveloped bool
	// Sent is false when the failure came before the request could have reached the module (the connection was refused, the name did not resolve, the TLS handshake failed,
	// the request was refused here), and true otherwise: after a timeout or a reset the module may or may not have acted on it. An adapter that submits a write the module cannot
	// deduplicate (module 1's synchronous verification) must treat Sent as "the outcome is unknown"; a write that carries the module's idempotency key (module 1's bulk
	// submission) is simply sent again with the same key.
	Sent bool
	// Attempts is the number of attempts made (1 unless the request was retried).
	Attempts int
	// Cause is the underlying error, for errors.Is and errors.As only.
	Cause error
}

// Error renders the facts and nothing from a response body.
func (e *Error) Error() string {
	if e == nil {
		return "modclient: <nil>"
	}
	var b strings.Builder
	b.WriteString("modclient: ")
	if e.Module != "" {
		b.WriteString(string(e.Module))
		b.WriteByte(' ')
	}
	if e.Route != "" {
		b.WriteString(e.Route)
		b.WriteString(": ")
	}
	b.WriteString(e.Kind.String())
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	var facts []string
	if e.Status != 0 {
		facts = append(facts, "HTTP "+strconv.Itoa(e.Status))
	}
	if e.Code != "" {
		facts = append(facts, "code "+e.Code)
	}
	if e.Key != "" {
		facts = append(facts, "key "+e.Key)
	}
	if e.RequestID != "" {
		facts = append(facts, "request_id "+e.RequestID)
	}
	if e.Attempts > 1 {
		facts = append(facts, strconv.Itoa(e.Attempts)+" attempts")
	}
	if len(facts) > 0 {
		b.WriteString(" (")
		b.WriteString(strings.Join(facts, ", "))
		b.WriteByte(')')
	}
	return b.String()
}

// Unwrap gives the cause.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// Is makes errors.Is(err, ErrUnavailable) (and its siblings) true for an *Error of that Kind.
func (e *Error) Is(target error) bool {
	k, ok := target.(kindSentinel)
	return ok && e != nil && e.Kind == Kind(k)
}

// Format renders the error the same way for every verb (a %d or %x of a struct would otherwise print its fields).
func (e *Error) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, e.Error()) }

// GoString is what %#v prints.
func (e *Error) GoString() string { return e.Error() }

// Retryable reports whether err is an *Error of a kind the client itself retries for an idempotent request: Unavailable, Timeout or RateLimited. It is false for Paused (wait,
// do not hammer), for every answer of the module (Refused, NotFound, Internal, Contract) and for Canceled.
func Retryable(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Kind.retryable()
}

// KindOf returns the Kind of err, or 0 when err is not (and does not wrap) an *Error.
func KindOf(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return 0
}

// safeCause carries an underlying error for errors.Is and errors.As while printing only a fixed class word: a transport error's own text names addresses, and an injected
// RoundTripper's can name anything it was given. It deliberately has NO Unwrap method: a caller that walks the chain with errors.Unwrap and prints every link would otherwise
// reach the raw text. errors.Is(err, context.DeadlineExceeded) and errors.As(err, &netErr) still work, through Is and As below.
type safeCause struct {
	class string
	err   error
}

func (c *safeCause) Error() string { return c.class }

func (c *safeCause) Is(target error) bool { return errors.Is(c.err, target) }

func (c *safeCause) As(target any) bool { return errors.As(c.err, target) }

func cause(class string, err error) error {
	if err == nil {
		return nil
	}
	return &safeCause{class: class, err: err}
}
