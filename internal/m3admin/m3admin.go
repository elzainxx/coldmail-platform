// Package m3admin is the platform's client for module 3's per-user SUSPEND admin routes (module 3's
// admin domain B, Docs/history/p2-research/m3-suspend.txt has every fact below with its file:line in
// module 3 at the pinned commit):
//
//	POST {base}/v1/admin/users/{id}/suspend     200 {"user_id","suspended":true,"reason":"operator"}
//	POST {base}/v1/admin/users/{id}/unsuspend   200 {"user_id","suspended":false}
//	GET  {base}/v1/admin/users/{id}/config      200 the user exists, 404 it does not (an existence probe only)
//	GET  {base}/v1/admin/users/{id}/suspension  200 {"user_id","suspended","suspended_at","reinstated_at","version"}
//
// The last route is module 3's suspension READ route (module 3 238bd0a, Docs/api-contract.md "Reading a
// user's suspension state"): Suspension reads the state back, which is what the platform's reconciler
// treats as the confirmation of a convergence. The POST routes answer a change (200) or "already in that
// state" (422); only this route says what state the user is in.
//
// The credential is module 3's ADMIN key (Authorization: Bearer), a different credential from the
// platform service token.
//
// WHAT THE CLIENT PROMISES. SetSuspended returns a Result only when module 3 ITSELF said what state the
// user is in now, and never a state it was not told:
//
//   - 200 whose JSON agrees with what was asked: the state CHANGED on this call (Changed true).
//   - 422 carrying {field:"suspended", issue:"conflict"} in error.details: the state was ALREADY as asked
//     (Changed false). Module 3's status codes are not idempotent but its effects are, so a retry after a
//     lost response converges: the first call's 200 is gone, the second call's 422 says the same thing.
//   - Anything else is an error. A 200 whose "suspended" disagrees with the request, or whose body is not
//     the documented shape, is a *RetryableError and NEVER a Result; so is any 422 without the conflict
//     detail. The caller cannot be talked into recording a state that module 3 did not report.
//
// UNSUSPEND HAS A TRAP. Module 3 does not check that the user exists on that route, so an unknown user
// answers the same 422 conflict as a user who is simply not suspended. The 422 is therefore confirmed as
// StateActive only after GET .../config says 200; a 404 there is ErrUnknownUser.
//
// WHAT SUSPENSION PROMISES. Suspension returns a SuspensionState only for a 200 whose JSON carries the five
// documented keys with their documented types (user_id equal to the id asked for, suspended a boolean,
// suspended_at and reinstated_at an RFC 3339 instant or null, version a non-negative integer); a key that is
// missing, a wrong type, a user id that is not the one asked for or a body that is not one JSON object is a
// *RetryableError and NEVER a state. A key the contract does not list is ignored (module 3's API is additive).
// A 404 is ErrUnknownUser (module 3's own not_found envelope, as for the POST routes), never "not
// suspended": a user module 3 does not know has no state to report, and the caller must not read it as an
// active one. A user module 3 knows but never suspended is a 200 with suspended false and version 0.
//
// ERRORS (use errors.Is / errors.As):
//
//	ErrUnauthorized  401: the admin key is wrong or revoked. A configuration fault: never retry in a loop.
//	ErrUnknownUser   module 3 does not know the user (404 on suspend, the suspension read or the existence probe).
//	ErrInvalid       400 from module 3, or an id/reason refused here BEFORE any request was made.
//	*RetryableError  nothing was confirmed: a network error, a timeout, 5xx, 429, a redirect, a status or a
//	                 body that is not the documented shape. Retryable(err) is the test for it.
//
// A 404 counts as ErrUnknownUser only when its body is module 3's own not_found envelope. A bare 404 (the
// wrong base URL, a proxy, a gateway page) is a *RetryableError: concluding "no such user, nothing to
// suspend" from a response that did not come from module 3 would be the unsafe direction for a halt. The
// price is that module 3's own "admin surface not mounted" 404 (ADMIN_API_KEY_HASHES unset) is the same
// not_found envelope and so still reads as an unknown user; module 3 gives the two no distinguishing
// mark.
//
// SECRETS. The admin key is a config.Secret and is revealed at exactly one place, the Authorization
// header. It is in no error string: an error carries a status, a fixed reason and, from module 3's
// error envelope, only a code from module 3's closed vocabulary and a request id of module 3's own shape,
// both dropped if they contain the key. Response bodies are never quoted. The Client renders as
// "[redacted]" for every fmt verb: a struct field of type config.Secret is NOT enough on its own, because
// fmt does not call methods on unexported fields and would print the raw string, so Client implements
// Format itself.
//
// TRANSPORT. The request goes to the base URL and nowhere else: redirects are never followed, whatever
// client is supplied (a 307 would re-send the credential and the body to wherever it points), and the
// default client does not use HTTP_PROXY (an admin credential must not be handed to a proxy). Bodies are
// read through a hard 1 MiB cap. A Client is immutable once built.
package m3admin

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/coldmail-platform/platform/internal/config"
)

const (
	// MaxBodyBytes is the most of a response body that is read. A longer body is never trusted.
	MaxBodyBytes = 1 << 20
	// MaxReasonBytes is module 3's limit on the optional reason (user_suspend.go: len(*req.Reason) > 2000).
	MaxReasonBytes = 2000
	// DefaultTimeout is the timeout of the client New builds when none is supplied.
	DefaultTimeout = 10 * time.Second
)

// systemUserID is module 2's shared-pool owner. It can hold nothing and is never suspended.
const systemUserID = "00000000-0000-0000-0000-000000000000"

// State is the state module 3 reported for a user.
type State string

const (
	StateSuspended State = "suspended"
	StateActive    State = "active"
)

// Result is what module 3 confirmed.
type Result struct {
	// State is the state module 3 reports for the user now.
	State State
	// Changed is true when module 3 changed the state on this call (a 200), false when it already was in
	// that state (a 422 conflict). Either way State is true of module 3 now.
	Changed bool
	// HTTPStatus is the status of the response that decided the outcome: the 200, or the 422 conflict. An
	// unsuspend 422 is then confirmed by the existence probe (a 200 there), but it is the 422 that told us
	// the state, so it is the 422 that is reported.
	HTTPStatus int
}

var (
	// ErrUnauthorized: module 3 answered 401. The admin key is wrong or revoked; retrying will not help.
	ErrUnauthorized = errors.New("m3admin: module 3 refused the admin key")
	// ErrUnknownUser: module 3 does not know this user.
	ErrUnknownUser = errors.New("m3admin: module 3 does not know this user")
	// ErrInvalid: module 3 answered 400, or the user id or reason was refused before any request.
	ErrInvalid = errors.New("m3admin: the request is invalid")
)

// RetryableError means nothing was confirmed and asking again is the right response. Because module 3's
// effects are idempotent, asking again converges even when the first call did change the state.
type RetryableError struct {
	Op         string // "suspend", "unsuspend", "unsuspend (existence probe)" or "suspension read"
	HTTPStatus int    // the status that was received, 0 when there was no response
	Reason     string // a fixed phrase written here; never text from the response
	Cause      error  // the underlying error, if any; reachable with errors.Is / errors.As, never printed
}

func (e *RetryableError) Error() string {
	var b strings.Builder
	b.WriteString("m3admin: ")
	b.WriteString(e.Op)
	b.WriteString(": ")
	b.WriteString(e.Reason)
	if e.HTTPStatus != 0 {
		fmt.Fprintf(&b, " (HTTP %d)", e.HTTPStatus)
	}
	b.WriteString("; nothing was confirmed, retry later")
	return b.String()
}

// Unwrap gives the cause (a net error, context.DeadlineExceeded, ...), nil when there is none.
func (e *RetryableError) Unwrap() error { return e.Cause }

// Retryable reports whether err is, or wraps, a *RetryableError.
func Retryable(err error) bool {
	var r *RetryableError
	return errors.As(err, &r)
}

// Client talks to one module 3 Control. Build it with New.
type Client struct {
	base  string
	token config.Secret
	hc    *http.Client
}

// New builds a Client. baseURL must be http or https with a host (a path prefix is allowed; a trailing
// slash is trimmed) and must carry no credentials, query or fragment: a credential in a URL is exactly
// what ends up in a log line, so it is refused rather than carried. token is module 3's admin key. hc may
// be nil, in which case the client has a 10 s timeout, no proxy and one connection per request; a
// supplied client is copied (the caller's is never modified) and its CheckRedirect is replaced so that no
// redirect is ever followed.
//
// Errors from New never contain the URL or the token.
func New(baseURL string, token config.Secret, hc *http.Client) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.Opaque != "" ||
		u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, errors.New("m3admin: the base URL must be an http(s) URL with a host and no credentials, query or fragment")
	}
	tok := token.Reveal()
	if tok == "" {
		return nil, errors.New("m3admin: the admin key is empty")
	}
	for i := 0; i < len(tok); i++ {
		if tok[i] <= 0x20 || tok[i] >= 0x7f {
			return nil, errors.New("m3admin: the admin key contains whitespace, a control character or a non-ASCII character (a trailing newline from a secrets file?)")
		}
	}
	var cp http.Client
	if hc == nil {
		cp = *defaultHTTPClient()
	} else {
		cp = *hc
	}
	cp.CheckRedirect = noRedirect
	return &Client{
		base:  u.Scheme + "://" + u.Host + strings.TrimRight(u.EscapedPath(), "/"),
		token: token,
		hc:    &cp,
	}, nil
}

func defaultHTTPClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.DisableKeepAlives = true
	return &http.Client{Timeout: DefaultTimeout, Transport: tr}
}

func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// Format renders the Client without its credential for EVERY verb. The base URL is shown (New refused any
// credential in it). A value receiver, so a Client and a *Client both render this way.
func (c Client) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, c.String()) }

func (c Client) String() string {
	return "m3admin.Client{base: " + c.base + ", token: [redacted]}"
}

func (c Client) GoString() string { return c.String() }

// SetSuspended asks module 3 to suspend (suspended true) or unsuspend (false) the user, and returns what
// module 3 confirmed. reason is optional (empty sends no body); it is stored by module 3 only as an audit
// note. See the package comment for the full contract.
func (c *Client) SetSuspended(ctx context.Context, userID string, suspended bool, reason string) (Result, error) {
	op, want := "unsuspend", StateActive
	if suspended {
		op, want = "suspend", StateSuspended
	}
	if why := validateUserID(userID); why != "" {
		return Result{}, fmt.Errorf("%w: %s: %s", ErrInvalid, op, why)
	}
	if why := validateReason(reason); why != "" {
		return Result{}, fmt.Errorf("%w: %s: %s", ErrInvalid, op, why)
	}
	var body []byte
	if reason != "" {
		var err error
		if body, err = json.Marshal(struct {
			Reason string `json:"reason"`
		}{reason}); err != nil {
			return Result{}, fmt.Errorf("%w: %s: the reason could not be encoded", ErrInvalid, op)
		}
	}

	r, err := c.send(ctx, op, http.MethodPost, "/v1/admin/users/"+userID+"/"+op, body)
	if err != nil {
		return Result{}, err
	}
	switch r.status {
	case http.StatusOK:
		return c.confirmChange(op, userID, suspended, r)
	case http.StatusUnprocessableEntity:
		env := parseEnvelope(r)
		if !env.conflict {
			return Result{}, c.retryable(op, r.status,
				"a 422 without the documented {field: suspended, issue: conflict} detail"+c.where(env))
		}
		if !suspended {
			// "not suspended" and "no such user" are the same 422 on this route.
			if err := c.requireKnown(ctx, userID); err != nil {
				return Result{}, err
			}
		}
		return Result{State: want, Changed: false, HTTPStatus: r.status}, nil
	default:
		return Result{}, c.failure(op, r)
	}
}

// confirmChange accepts a 200 only when its JSON is the documented shape, names this user and says the
// state that was asked for.
func (c *Client) confirmChange(op, userID string, suspended bool, r response) (Result, error) {
	if r.oversized {
		return Result{}, c.retryable(op, r.status, "the 200 body is larger than 1 MiB")
	}
	var ok struct {
		UserID    *string `json:"user_id"`
		Suspended *bool   `json:"suspended"`
	}
	if !decodeOne(r.body, &ok) || ok.UserID == nil || ok.Suspended == nil {
		return Result{}, c.retryable(op, r.status, "the 200 body is not the documented {user_id, suspended} shape")
	}
	if *ok.UserID != userID {
		return Result{}, c.retryable(op, r.status, "the 200 body names a different user")
	}
	if *ok.Suspended != suspended {
		return Result{}, c.retryable(op, r.status,
			fmt.Sprintf("module 3 answered 200 with suspended=%t but suspended=%t was asked", *ok.Suspended, suspended))
	}
	state := StateActive
	if suspended {
		state = StateSuspended
	}
	return Result{State: state, Changed: true, HTTPStatus: r.status}, nil
}

// requireKnown is the existence probe behind an unsuspend 422: a 200 means the user exists, so the 422
// really was "not suspended"; a 404 from module 3 means the user is unknown.
func (c *Client) requireKnown(ctx context.Context, userID string) error {
	const op = "unsuspend (existence probe)"
	r, err := c.send(ctx, op, http.MethodGet, "/v1/admin/users/"+userID+"/config", nil)
	if err != nil {
		return err
	}
	if r.status == http.StatusOK {
		return nil
	}
	return c.failure(op, r)
}

// ---- the suspension read ----------------------------------------------------------------------------------------

// opSuspensionRead is the Op of every error of Suspension.
const opSuspensionRead = "suspension read"

// SuspensionState is what module 3's suspension read route answered for one user (module 3 Docs/api-contract.md,
// "Reading a user's suspension state"). A user module 3 knows but has never suspended reads Suspended false, both
// timestamps nil and Version 0.
type SuspensionState struct {
	// User is the user id module 3 echoed: always the one that was asked for (anything else is an error).
	User string
	// Suspended is whether the user is suspended right now.
	Suspended bool
	// SuspendedAt is when the most recent suspension began (UTC); it is kept after a reinstatement. Nil if never suspended.
	SuspendedAt *time.Time
	// ReinstatedAt is when the most recent reinstatement happened (UTC); it is kept by a later re-suspension. Nil if never reinstated.
	ReinstatedAt *time.Time
	// Version is module 3's own counter of suspension-state transitions for this user: 0 for a user with no row,
	// then 1 for the first suspend, 2 for the unsuspend after it, and so on. It never decreases.
	Version int64
}

// State is the state the read reports, in the vocabulary of SetSuspended.
func (s SuspensionState) State() State {
	if s.Suspended {
		return StateSuspended
	}
	return StateActive
}

// Suspension reads the user's suspension state from module 3 (GET /v1/admin/users/{id}/suspension). It returns a
// state only when module 3 itself answered 200 with the documented body for this user (see the package comment);
// an unknown user is ErrUnknownUser and never an "active" answer, and a failure, an unreadable or oversized body or
// a body for another user is an error (a *RetryableError unless it is one of the typed errors) and never a state.
func (c *Client) Suspension(ctx context.Context, userID string) (SuspensionState, error) {
	if why := validateUserID(userID); why != "" {
		return SuspensionState{}, fmt.Errorf("%w: %s: %s", ErrInvalid, opSuspensionRead, why)
	}
	r, err := c.send(ctx, opSuspensionRead, http.MethodGet, "/v1/admin/users/"+userID+"/suspension", nil)
	if err != nil {
		return SuspensionState{}, err
	}
	if r.status != http.StatusOK {
		return SuspensionState{}, c.failure(opSuspensionRead, r)
	}
	return c.confirmSuspension(userID, r)
}

// confirmSuspension accepts a 200 only when its JSON is one object with the five documented keys, each of its
// documented type, naming the user that was asked for.
func (c *Client) confirmSuspension(userID string, r response) (SuspensionState, error) {
	const op = opSuspensionRead
	if r.oversized {
		return SuspensionState{}, c.retryable(op, r.status, "the 200 body is larger than 1 MiB")
	}
	notShape := func(what string) (SuspensionState, error) {
		return SuspensionState{}, c.retryable(op, r.status, "the 200 body is not the documented {user_id, suspended, suspended_at, reinstated_at, version} shape: "+what)
	}
	// RawMessage keeps an absent key apart from a null one, and a number apart from a quoted string.
	var body struct {
		UserID       *string         `json:"user_id"`
		Suspended    *bool           `json:"suspended"`
		SuspendedAt  json.RawMessage `json:"suspended_at"`
		ReinstatedAt json.RawMessage `json:"reinstated_at"`
		Version      json.RawMessage `json:"version"`
	}
	if !decodeOne(r.body, &body) {
		return notShape("not one JSON object")
	}
	if body.UserID == nil || body.Suspended == nil {
		return notShape("user_id or suspended is missing or not the documented type")
	}
	if *body.UserID != userID {
		return SuspensionState{}, c.retryable(op, r.status, "the 200 body names a different user")
	}
	suspendedAt, ok := nullableInstant(body.SuspendedAt)
	if !ok {
		return notShape("suspended_at is missing or neither an RFC 3339 instant nor null")
	}
	reinstatedAt, ok := nullableInstant(body.ReinstatedAt)
	if !ok {
		return notShape("reinstated_at is missing or neither an RFC 3339 instant nor null")
	}
	version, ok := counter(body.Version)
	if !ok {
		return notShape("version is missing or not a non-negative integer")
	}
	return SuspensionState{User: *body.UserID, Suspended: *body.Suspended, SuspendedAt: suspendedAt, ReinstatedAt: reinstatedAt, Version: version}, nil
}

// nullableInstant reads a JSON value that must be present and be either null (nil, true) or an RFC 3339 string
// (the instant in UTC, true). Absent, any other type or an unparseable string is false.
func nullableInstant(raw json.RawMessage) (*time.Time, bool) {
	raw = bytes.TrimSpace(raw)
	switch {
	case len(raw) == 0:
		return nil, false
	case string(raw) == "null":
		return nil, true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return nil, false
	}
	t = t.UTC()
	return &t, true
}

// counter reads a JSON value that must be a bare integer token that fits an int64 and is not negative: a quoted
// "1", 1.5, 1e3, null, an absent key and a negative number are all refused.
func counter(raw json.RawMessage) (int64, bool) {
	n, err := strconv.ParseInt(string(bytes.TrimSpace(raw)), 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// ---- one request ------------------------------------------------------------------------------------------------

// response is one answered request. body is at most MaxBodyBytes; oversized says it was cut there.
type response struct {
	status    int
	body      []byte
	oversized bool
}

// send makes one request. A transport failure or an unreadable body comes back as a *RetryableError; any
// answered request comes back as a response, whatever its status.
func (c *Client) send(ctx context.Context, op, method, path string, body []byte) (response, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return response{}, fmt.Errorf("%w: %s: the request could not be built", ErrInvalid, op)
	}
	req.Header.Set("Authorization", "Bearer "+c.token.Reveal())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "platform-m3admin")
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return response{}, &RetryableError{Op: op, Reason: "the request failed: " + classify(err), Cause: stripURL(err)}
	}
	defer func() { _ = resp.Body.Close() }()

	b, err := io.ReadAll(io.LimitReader(resp.Body, MaxBodyBytes+1))
	if err != nil {
		return response{}, &RetryableError{Op: op, HTTPStatus: resp.StatusCode,
			Reason: "reading the response body failed: " + classify(err), Cause: err}
	}
	r := response{status: resp.StatusCode, body: b}
	if len(b) > MaxBodyBytes {
		r.body, r.oversized = b[:MaxBodyBytes], true
	}
	return r, nil
}

func (c *Client) retryable(op string, status int, reason string) *RetryableError {
	return &RetryableError{Op: op, HTTPStatus: status, Reason: reason}
}

// failure maps a response that is not a success to the error class it belongs to. It never quotes the
// body.
func (c *Client) failure(op string, r response) error {
	env := parseEnvelope(r)
	where := c.where(env)
	switch {
	case r.status == http.StatusUnauthorized:
		return fmt.Errorf("%w: %s: HTTP 401%s", ErrUnauthorized, op, where)
	case r.status == http.StatusBadRequest:
		return fmt.Errorf("%w: %s: module 3 answered HTTP 400%s", ErrInvalid, op, where)
	case r.status == http.StatusNotFound:
		if env.ok && env.code == codeNotFound {
			return fmt.Errorf("%w: %s: HTTP 404%s", ErrUnknownUser, op, where)
		}
		return c.retryable(op, r.status,
			"a 404 that is not module 3's not_found envelope (the wrong base URL, or something in front of module 3)"+where)
	case r.status == http.StatusTooManyRequests:
		return c.retryable(op, r.status, "rate limited"+where)
	case r.status >= 500:
		return c.retryable(op, r.status, "a server error"+where)
	case r.status >= 300 && r.status < 400:
		return c.retryable(op, r.status, "a redirect, which is never followed (is the base URL module 3's Control address?)")
	case r.status >= 200 && r.status < 300:
		return c.retryable(op, r.status, "a success status module 3 does not document for this route"+where)
	}
	return c.retryable(op, r.status, "a status module 3 does not document for this route"+where)
}

// ---- module 3's error envelope --------------------------------------------------------------------------------

const codeNotFound = "not_found"

// knownCodes is module 3's closed, additive-only error vocabulary (adminapi/errors.go). Only these may be
// repeated in an error string.
var knownCodes = map[string]bool{
	"validation_failed": true, "config_ordering_violation": true, "unauthenticated": true, "forbidden": true,
	"not_found": true, "method_not_allowed": true, "conflict": true, "unprocessable": true,
	"rate_limited": true, "internal_error": true,
}

// envelope is what could be read of {"error":{"code","message","details":[{field,issue}],"request_id"}}.
type envelope struct {
	ok        bool // the body was a decodable error envelope
	code      string
	requestID string
	conflict  bool // error.details has {field: "suspended", issue: "conflict"}
}

func parseEnvelope(r response) envelope {
	if r.oversized {
		return envelope{}
	}
	var e struct {
		Error *struct {
			Code    string `json:"code"`
			Details []struct {
				Field string `json:"field"`
				Issue string `json:"issue"`
			} `json:"details"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	if !decodeOne(r.body, &e) || e.Error == nil {
		return envelope{}
	}
	out := envelope{ok: true, code: e.Error.Code, requestID: e.Error.RequestID}
	for _, d := range e.Error.Details {
		if d.Field == "suspended" && d.Issue == "conflict" {
			out.conflict = true
		}
	}
	return out
}

// where renders the parts of an envelope that are safe to put in an error string: a code from module 3's
// closed vocabulary and a request id of module 3's shape ("req_" and hex), each only if it does not
// contain the admin key. Anything else in the response is dropped, so a response that echoes the
// Authorization header into every field still puts nothing of it in an error.
func (c *Client) where(e envelope) string {
	var parts []string
	if knownCodes[e.code] && !c.hasToken(e.code) {
		parts = append(parts, "code "+e.code)
	}
	if isRequestID(e.requestID) && !c.hasToken(e.requestID) {
		parts = append(parts, "request_id "+e.requestID)
	}
	if len(parts) == 0 {
		return ""
	}
	return " [" + strings.Join(parts, ", ") + "]"
}

func (c *Client) hasToken(s string) bool { return strings.Contains(s, c.token.Reveal()) }

// isRequestID accepts module 3's request id shape: "req_" and 16 to 64 lower-case hex digits.
func isRequestID(s string) bool {
	const prefix = "req_"
	if !strings.HasPrefix(s, prefix) || len(s) < len(prefix)+16 || len(s) > len(prefix)+64 {
		return false
	}
	for i := len(prefix); i < len(s); i++ {
		if b := s[i]; !(b >= '0' && b <= '9' || b >= 'a' && b <= 'f') {
			return false
		}
	}
	return true
}

// decodeOne decodes b as exactly one JSON value into v: an empty body, a body that is not JSON and a body
// with anything after the value all fail.
func decodeOne(b []byte, v any) bool {
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(v); err != nil {
		return false
	}
	_, err := dec.Token()
	return err == io.EOF
}

// ---- validation before any request -------------------------------------------------------------------------------

// validateUserID returns why id is refused, or "" when it is acceptable: a canonical lower-case hyphenated
// uuid that is not the system user. Module 3 accepts upper case and echoes it, and answers a malformed id
// with a 404 that cannot be told from an unknown user, so neither is sent.
func validateUserID(id string) string {
	switch {
	case id == "":
		return "the user id is empty"
	case id == systemUserID:
		return "the user id is the system user (all zeros), which is never suspended"
	case len(id) != 36:
		return "the user id is not a canonical lower-case uuid"
	}
	for i := 0; i < len(id); i++ {
		b := id[i]
		switch i {
		case 8, 13, 18, 23:
			if b != '-' {
				return "the user id is not a canonical lower-case uuid"
			}
		default:
			if !(b >= '0' && b <= '9' || b >= 'a' && b <= 'f') {
				return "the user id is not a canonical lower-case uuid"
			}
		}
	}
	return ""
}

// validateReason returns why reason is refused, or "". Module 3 limits it to 2000 BYTES; control
// characters (newlines included) and invalid UTF-8 are refused so an audit note is always one clean line.
func validateReason(reason string) string {
	if len(reason) > MaxReasonBytes {
		return "the reason is longer than 2000 bytes"
	}
	if !utf8.ValidString(reason) {
		return "the reason is not valid UTF-8"
	}
	for _, r := range reason {
		if unicode.IsControl(r) {
			return "the reason contains a control character"
		}
	}
	return ""
}

// ---- transport errors -------------------------------------------------------------------------------------------

// stripURL drops the *url.Error wrapper, whose text carries the request URL, and keeps the error beneath
// it (errors.Is(err, context.DeadlineExceeded) and net.Error still work).
func stripURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err
	}
	return err
}

// classify names the class of a transport error: "timeout", "connection refused" and so on. It never
// returns err.Error(): the text of a *url.Error carries the full URL and the text of other errors carries
// addresses and paths. The unclassified case names the Go type of the innermost error.
func classify(err error) string {
	var dns *net.DNSError
	var certErr *tls.CertificateVerificationError
	var unknownAuth x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	var ne net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case isRefused(err):
		return "connection refused"
	case errors.As(err, &dns):
		if dns.IsNotFound {
			return "dns lookup failed (no such host)"
		}
		return "dns lookup failed"
	case isReset(err):
		return "connection reset"
	case errors.As(err, &certErr), errors.As(err, &unknownAuth), errors.As(err, &hostErr):
		return "tls certificate rejected"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "connection closed before a complete response"
	}
	root := err
	for {
		next := errors.Unwrap(root)
		if next == nil {
			break
		}
		root = next
	}
	return fmt.Sprintf("network error (%T)", root)
}

// Windows reports a refused or reset connection as WSAECONNREFUSED / WSAECONNRESET, which
// errors.Is(err, syscall.ECONNREFUSED) does not match there, so the numbers are checked as well.
const (
	wsaeConnReset   = 10054
	wsaeConnRefused = 10061
)

func isRefused(err error) bool {
	if errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	var errno syscall.Errno
	if errors.As(err, &errno) && uintptr(errno) == wsaeConnRefused {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "refused")
}

func isReset(err error) bool {
	if errors.Is(err, syscall.ECONNRESET) {
		return true
	}
	var errno syscall.Errno
	if errors.As(err, &errno) && uintptr(errno) == wsaeConnReset {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "connection reset") || strings.Contains(msg, "forcibly closed")
}
