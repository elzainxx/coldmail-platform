// Package modclient is the one plumbing layer the three module adapters share (Phase 3, chunk B1): an HTTP client for a module's Domain A (or, for module 1, its
// per-caller API) with the bearer secret kept out of every error and log line, the module's error envelope parsed into typed errors, a size limit on every response,
// per-request timeouts, and retries that happen only where a retry cannot do harm.
//
// # WHAT IT PROMISES, AND WHAT IT DOES NOT KNOW
//
// Every method returns (value, error) and on any error the value is nil. There is no fallback value anywhere: a module that is down, slow, paused, refusing, faulty or
// answering something this client was not told to expect yields a typed *Error and nothing else, so no adapter can turn "the module did not answer" into a verdict, a zero
// count or a settled item. The client knows the shared error envelope (its codes and what each status means) and, through Decode and Shape, how to demand that a response
// has the keys and types an adapter relies on. It knows nothing else about a module: routes, tenancy, states and prices live in the adapters.
//
// # ERRORS
//
// One type, *Error, with a Kind; errors.Is(err, modclient.ErrUnavailable) and its siblings, and errors.As(err, &e) for the details. The Kinds and what produces each are
// documented on the constants (errors.go). A request is retried by the client only when the caller marked it Idempotent AND the failure was Unavailable, Timeout or
// RateLimited, up to Options.Retries extra attempts with an exponential, jitter-free wait; the caller's context ends everything at once. Retrying a request that is not marked
// idempotent, or retrying after any answer of the module (a refusal, a not found, a 500, a contract violation), never happens whatever the dials say.
//
// # SECRETS
//
// The bearer secret is a config.Secret revealed at exactly one place, the Authorization header of the request. It appears in no error string (an Error holds only a status,
// the module's code, request id and details restricted to a safe alphabet and dropped if they contain the secret, and sentences written here), no log line and no %v / %+v /
// %#v / %d of a Client, a Request or an Error: each of the three renders itself, because fmt does not call methods on an unexported field and a plain struct holding a
// config.Secret would print the raw string.
//
// # TRANSPORT
//
// The client runs over an injected *http.Client (never http.DefaultClient: a nil one is replaced by a fresh client with its own transport and no proxy), which is COPIED, and
// the copy's CheckRedirect is replaced so no redirect is ever followed (a 307 would re-send the credential and the body to wherever it points): a 3xx is a Contract error.
// Response bodies are read through a hard size limit. Each attempt has its own deadline (Options.Timeout, or Request.Timeout when the call sets one), which covers the
// response body too, so a server that sends its headers and stalls is a Timeout, not a hang.
package modclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coldmail-platform/platform/internal/config"
)

const (
	// DefaultTimeout is one attempt's budget when Options.Timeout is 0 (the default of PLATFORM_MODULE_TIMEOUT_MS).
	DefaultTimeout = 15 * time.Second
	// DefaultRetries is the default of PLATFORM_MODULE_RETRIES.
	DefaultRetries = 2
	// DefaultBackoff is the default of PLATFORM_MODULE_RETRY_BACKOFF_MS.
	DefaultBackoff = 250 * time.Millisecond
	// MaxBackoff caps the exponential wait between attempts (and is the largest accepted Options.Backoff).
	MaxBackoff = 30 * time.Second
	// DefaultMaxBodyBytes is the most of a response body that is read when Options.MaxBodyBytes is 0: a page of module 1's largest results listing (1000 verdicts of about
	// a kilobyte each) fits with room to spare.
	DefaultMaxBodyBytes = 8 << 20
	// MaxRequestTimeout is the longest per-request override a call may ask for (module 3's bulk add of 500 accounts paces itself at seconds per row).
	MaxRequestTimeout = 6 * time.Hour

	maxRetries = 100
	userAgent  = "platform-modclient"
)

// Options are the dials of a Client. The zero value is valid: the default timeout and body limit and no retries (so a zero Retries is "none", the dial's own 0). FromConfig and
// DefaultOptions build the usual ones.
type Options struct {
	// Timeout is one attempt's budget, response body included; 0 means DefaultTimeout. PLATFORM_MODULE_TIMEOUT_MS.
	Timeout time.Duration
	// Retries is the number of extra attempts an Idempotent request gets after a retryable failure; 0 means none. PLATFORM_MODULE_RETRIES.
	Retries int
	// Backoff is the wait before the first retry; each further retry waits twice as long, up to MaxBackoff; 0 retries at once. No jitter, so a run is reproducible.
	// PLATFORM_MODULE_RETRY_BACKOFF_MS.
	Backoff time.Duration
	// MaxBodyBytes is the most of one response body that is read; 0 means DefaultMaxBodyBytes. A longer body is a Contract error (on a success status) and is never parsed.
	MaxBodyBytes int64
	// Sleep waits between attempts and returns the context's error if it ends first; nil means a real timer. It exists so a test can observe the waits without sleeping.
	Sleep func(ctx context.Context, d time.Duration) error
}

// DefaultOptions are the documented defaults of the three dials (Docs/config-reference.md). A test pins them equal to what config.Load yields for an empty environment.
func DefaultOptions() Options {
	return Options{Timeout: DefaultTimeout, Retries: DefaultRetries, Backoff: DefaultBackoff, MaxBodyBytes: DefaultMaxBodyBytes}
}

// FromConfig is the consumer of PLATFORM_MODULE_TIMEOUT_MS, PLATFORM_MODULE_RETRIES and PLATFORM_MODULE_RETRY_BACKOFF_MS: it is what every adapter builds its Client with.
// A Config that did not come from config.Load (the zero Config) yields the zero Options, which New completes with the default timeout and body limit and no retries.
func FromConfig(cfg config.Config) Options {
	return Options{Timeout: cfg.ModuleTimeout, Retries: cfg.ModuleRetries, Backoff: cfg.ModuleRetryBackoff}
}

// Request is one call.
type Request struct {
	// Module, if set, must be the module the Client was built for (a request built for module 2 and handed to module 3's client is refused before anything is sent).
	Module Module
	// Route is a stable label of the call: lower-case letters, digits, '_' and '.', for example "m2.report" or "m3.accounts.create". It is what an error names, so it must
	// carry no id and no address.
	Route string
	// Method is GET, POST, PUT, PATCH or DELETE.
	Method string
	// Path starts with '/' and is already URL-escaped (url.PathEscape each id); it carries no query (use Query).
	Path  string
	Query url.Values
	// Body, when non-nil, is sent as JSON (encoding/json). A GET or DELETE carries none.
	Body any
	// Idempotent says that sending this request twice has the effect of sending it once (a read; module 2 and 3's run, which the module deduplicates by job_item_id). Only
	// then is a failed request retried. Module 1's bulk submission is NOT idempotent: leave it false.
	Idempotent bool
	// Timeout, when positive, replaces Options.Timeout for this call's attempts (at most MaxRequestTimeout).
	Timeout time.Duration
	// Expect lists the success statuses of the route (each 2xx); empty means 200. Any other status is an error: a 2xx that is not listed is a Contract error.
	Expect []int
	// Anonymous sends no Authorization header (the modules' /healthz and /readyz need none, and a probe route must not be handed the credential).
	Anonymous bool
}

// Format renders a Request without its Body, Query or Path: a body can carry a credential (module 3's account creation carries an SMTP password) and a path an id.
func (r Request) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, r.String()) }

func (r Request) String() string {
	return "modclient.Request{" + string(r.Module) + " " + r.Route + " " + r.Method + " [path, query and body omitted]}"
}

func (r Request) GoString() string { return r.String() }

// LogValue implements slog.LogValuer: without it a JSON log handler would marshal the exported fields, Body included.
func (r Request) LogValue() slog.Value { return slog.StringValue(r.String()) }

// MarshalJSON renders the redacted form for the same reason: a Request that is JSON-encoded by accident must not carry its Body.
func (r Request) MarshalJSON() ([]byte, error) { return json.Marshal(r.String()) }

// Response is a successful answer: a status the route expects, a JSON content type (when there is a body) and a complete, well-formed JSON document within the size limit.
// Read it with Decode.
type Response struct {
	Status int
	Header http.Header
	// Body is the raw JSON document (empty for a status such as 204).
	Body []byte
	// RequestID is the module's own request id when the response carried one: an X-Request-Id header, else a top-level request_id of the body (module 3's bulk add). Most
	// success responses carry none ("" then); every error carries the envelope's.
	RequestID string
	// Attempts is the number of attempts it took (1 unless retried).
	Attempts int

	module Module
	route  string
}

// Client talks to one module. Build it with New; it is safe for concurrent use and immutable.
type Client struct {
	module Module
	base   string
	token  config.Secret
	hc     *http.Client
	opts   Options
}

// New builds a Client for module. baseURL must be http or https with a host and carry no credentials, query or fragment (a credential in a URL is what ends up in a log
// line, so it is refused, not carried; a path prefix is allowed and a trailing slash is trimmed). token is the bearer secret: module 1's per-caller key, or module 2's and
// 3's service token. hc is the transport: it is copied and its CheckRedirect replaced; nil gets a fresh client with its own transport and no proxy (a bearer secret must not be
// handed to a proxy). Errors from New never contain the URL or the token.
func New(module Module, baseURL string, token config.Secret, hc *http.Client, opts Options) (*Client, error) {
	bad := func(msg string) (*Client, error) {
		m := module
		if !m.valid() {
			m = ""
		}
		return nil, &Error{Kind: Invalid, Module: m, Route: "new", Message: msg}
	}
	if !module.valid() {
		return bad("the module must be m1, m2 or m3")
	}
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.Opaque != "" ||
		u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return bad("the base URL must be an http(s) URL with a host and no credentials, query or fragment")
	}
	tok := token.Reveal()
	if tok == "" {
		return bad("the bearer secret is empty")
	}
	for i := 0; i < len(tok); i++ {
		if tok[i] <= 0x20 || tok[i] >= 0x7f {
			return bad("the bearer secret contains whitespace, a control character or a non-ASCII character (a trailing newline from a secrets file?)")
		}
	}
	switch {
	case opts.Timeout < 0 || opts.Timeout > MaxRequestTimeout:
		return bad("the timeout must be positive and at most MaxRequestTimeout (0 means the default)")
	case opts.Retries < 0 || opts.Retries > maxRetries:
		return bad("the retry count is out of range")
	case opts.Backoff < 0 || opts.Backoff > MaxBackoff:
		return bad("the backoff must be between 0 and MaxBackoff")
	case opts.MaxBodyBytes < 0:
		return bad("the body limit must not be negative (0 means the default)")
	}
	if opts.Timeout == 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.MaxBodyBytes == 0 {
		opts.MaxBodyBytes = DefaultMaxBodyBytes
	}
	var cp http.Client
	if hc == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.Proxy = nil
		cp = http.Client{Transport: tr}
	} else {
		cp = *hc
	}
	cp.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{module: module, base: u.Scheme + "://" + u.Host + strings.TrimRight(u.EscapedPath(), "/"), token: token, hc: &cp, opts: opts}, nil
}

// Module is the module this client was built for.
func (c *Client) Module() Module { return c.module }

// CloseIdleConnections closes the transport's idle connections (the client keeps connections alive between calls).
func (c *Client) CloseIdleConnections() { c.hc.CloseIdleConnections() }

// Format renders the Client without its secret for EVERY verb (a struct field of type config.Secret is not enough on its own: fmt does not call methods on unexported fields, so
// it would print the raw string). A value receiver, so a Client and a *Client both render this way.
func (c Client) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, c.String()) }

func (c Client) String() string {
	return "modclient.Client{module: " + string(c.module) + ", base: " + c.base + ", token: [redacted]}"
}

func (c Client) GoString() string { return c.String() }

// LogValue implements slog.LogValuer.
func (c Client) LogValue() slog.Value { return slog.StringValue(c.String()) }

// ---- Do ---------------------------------------------------------------------------------------------------------------

// prepared is a Request that passed validation, with its defaults applied.
type prepared struct {
	route      string
	method     string
	target     string
	body       []byte
	timeout    time.Duration
	expect     []int
	idempotent bool
	anonymous  bool
}

func (p *prepared) expects(status int) bool {
	for _, s := range p.expect {
		if s == status {
			return true
		}
	}
	return false
}

func validRoute(s string) bool {
	if s == "" || len(s) > 64 || !(s[0] >= 'a' && s[0] <= 'z' || s[0] >= '0' && s[0] <= '9') {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

func validPath(p string) bool {
	if p == "" || len(p) > 2048 || p[0] != '/' {
		return false
	}
	for i := 0; i < len(p); i++ {
		if c := p[i]; c <= 0x20 || c >= 0x7f || c == '?' || c == '#' || c == '\\' {
			return false
		}
	}
	return true
}

func (c *Client) invalid(route, msg string) *Error {
	if !validRoute(route) {
		route = ""
	}
	return &Error{Kind: Invalid, Module: c.module, Route: route, Message: msg}
}

func (c *Client) prepare(req Request) (*prepared, *Error) {
	if !validRoute(req.Route) {
		return nil, c.invalid("", "the route label must be lower-case letters, digits, '_' and '.' (for example m2.report), at most 64 characters")
	}
	if req.Module != "" && req.Module != c.module {
		return nil, c.invalid(req.Route, "the request was built for another module than this client")
	}
	switch req.Method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return nil, c.invalid(req.Route, "the method must be GET, POST, PUT, PATCH or DELETE")
	}
	if !validPath(req.Path) {
		return nil, c.invalid(req.Route, "the path must start with '/', be URL-escaped and carry no query or fragment")
	}
	if req.Timeout < 0 || req.Timeout > MaxRequestTimeout {
		return nil, c.invalid(req.Route, "the per-request timeout must be positive and at most MaxRequestTimeout")
	}
	p := &prepared{route: req.Route, method: req.Method, timeout: c.opts.Timeout, idempotent: req.Idempotent, anonymous: req.Anonymous}
	if req.Timeout > 0 {
		p.timeout = req.Timeout
	}
	p.expect = req.Expect
	if len(p.expect) == 0 {
		p.expect = []int{http.StatusOK}
	}
	for _, s := range p.expect {
		if s < 200 || s > 299 {
			return nil, c.invalid(req.Route, "Expect lists success statuses: each must be 2xx")
		}
	}
	if req.Body != nil {
		if req.Method == http.MethodGet || req.Method == http.MethodDelete {
			return nil, c.invalid(req.Route, "a GET or DELETE carries no body")
		}
		b, err := json.Marshal(req.Body)
		if err != nil {
			return nil, c.invalid(req.Route, "the body could not be encoded as JSON")
		}
		p.body = b
	}
	p.target = c.base + req.Path
	if len(req.Query) > 0 {
		p.target += "?" + req.Query.Encode()
	}
	return p, nil
}

// Do makes one call and returns the module's success response, or an *Error. ON ANY ERROR THE RESPONSE IS NIL.
//
// A response is returned only for a status in req.Expect (200 when empty), a JSON content type, a complete JSON document (when there is a body) and a body within the size
// limit; everything else is an error of the Kind documented on the constants. A request marked Idempotent is retried after Unavailable, Timeout and RateLimited failures
// (Options.Retries times, with the exponential wait); the error returned is the LAST failure, with Attempts set. ctx ends the call, including a wait between attempts.
func (c *Client) Do(ctx context.Context, req Request) (*Response, error) {
	resp, e := c.do(ctx, req, c.judgeDefault)
	if e != nil {
		return nil, e
	}
	return resp, nil
}

// judgeFunc turns one raw answer into a Response or an error. Ready supplies its own, which accepts a 503 that carries a readiness document.
type judgeFunc func(p *prepared, raw *rawResponse) (*Response, *Error)

type rawResponse struct {
	status   int
	header   http.Header
	body     []byte
	oversize bool
}

func (c *Client) do(ctx context.Context, req Request, judge judgeFunc) (*Response, *Error) {
	p, e := c.prepare(req)
	if e != nil {
		return nil, e
	}
	attempts := 1
	if p.idempotent {
		attempts += c.opts.Retries
	}
	var last *Error
	for n := 1; n <= attempts; n++ {
		if err := ctx.Err(); err != nil {
			ce := c.ctxError(p, err, n > 1)
			ce.Attempts = n - 1
			return nil, ce
		}
		var resp *Response
		raw, e := c.roundTrip(ctx, p)
		if e == nil {
			resp, e = judge(p, raw)
		}
		if e == nil {
			resp.Attempts = n
			return resp, nil
		}
		e.Attempts = n
		last = e
		if n == attempts || !e.Kind.retryable() || ctx.Err() != nil {
			return nil, last
		}
		if err := c.sleep(ctx, c.waitFor(n, e, p.timeout)); err != nil {
			ce := c.ctxError(p, err, true)
			ce.Attempts = n
			return nil, ce
		}
	}
	return nil, last
}

// waitFor is the wait after the n-th failed attempt: Backoff doubled n-1 times, capped at MaxBackoff; for a 429, the larger of that and the module's Retry-After, never more than
// the request's timeout.
func (c *Client) waitFor(n int, e *Error, timeout time.Duration) time.Duration {
	d := c.opts.Backoff
	for i := 1; i < n && d < MaxBackoff; i++ {
		d *= 2
	}
	if d > MaxBackoff {
		d = MaxBackoff
	}
	if e.Kind == RateLimited {
		if e.RetryAfter > d {
			d = e.RetryAfter
		}
		if d > timeout {
			d = timeout
		}
	}
	return d
}

func (c *Client) sleep(ctx context.Context, d time.Duration) error {
	if c.opts.Sleep != nil {
		if err := c.opts.Sleep(ctx, d); err != nil {
			return err
		}
		return ctx.Err()
	}
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ctxError is the error for a call whose own context ended: Canceled, or Timeout when its deadline passed.
func (c *Client) ctxError(p *prepared, err error, sent bool) *Error {
	e := &Error{Module: c.module, Route: p.route, Sent: sent, Cause: cause("the caller's context ended", err)}
	if errors.Is(err, context.DeadlineExceeded) {
		e.Kind, e.Message = Timeout, "the caller's deadline passed before the call completed"
	} else {
		e.Kind, e.Message = Canceled, "the caller's context was canceled before the call completed"
	}
	return e
}

// roundTrip makes one attempt. Its error is a transport failure (nothing usable came back) or the caller's context ending; a response of any status is returned raw.
func (c *Client) roundTrip(ctx context.Context, p *prepared) (*rawResponse, *Error) {
	actx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	var rdr io.Reader
	if p.body != nil {
		rdr = bytes.NewReader(p.body)
	}
	hreq, err := http.NewRequestWithContext(actx, p.method, p.target, rdr)
	if err != nil {
		return nil, c.invalid(p.route, "the request could not be built")
	}
	hreq.Header.Set("Accept", "application/json")
	hreq.Header.Set("User-Agent", userAgent)
	if p.body != nil {
		hreq.Header.Set("Content-Type", "application/json")
	}
	if !p.anonymous {
		hreq.Header.Set("Authorization", "Bearer "+c.token.Reveal())
	}
	resp, err := c.hc.Do(hreq)
	if err != nil {
		return nil, c.transportError(ctx, actx, p, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, c.opts.MaxBodyBytes+1))
	if err != nil {
		return nil, c.transportError(ctx, actx, p, err)
	}
	raw := &rawResponse{status: resp.StatusCode, header: resp.Header.Clone()}
	if int64(len(b)) > c.opts.MaxBodyBytes {
		raw.oversize = true
	} else {
		raw.body = b
	}
	return raw, nil
}

func (c *Client) transportError(parent, actx context.Context, p *prepared, err error) *Error {
	if perr := parent.Err(); perr != nil {
		return c.ctxError(p, perr, true)
	}
	f := classifyTransport(err)
	if actx.Err() != nil {
		f.kind, f.reason = Timeout, "no complete response within the request timeout of "+p.timeout.String()
	}
	return &Error{Kind: f.kind, Module: c.module, Route: p.route, Message: f.reason, Sent: f.sent, Cause: cause(f.class, stripURL(err))}
}

// ---- judging a response ----------------------------------------------------------------------------------------------

func jsonContentType(h http.Header) bool {
	mt, _, err := mime.ParseMediaType(h.Get("Content-Type"))
	if err != nil {
		return false
	}
	return mt == "application/json" || strings.HasSuffix(mt, "+json")
}

// jsonProblem says why b is not exactly one complete JSON document, or "" when it is.
func jsonProblem(b []byte) string {
	dec := json.NewDecoder(bytes.NewReader(b))
	var v json.RawMessage
	if err := dec.Decode(&v); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return "the response body ends before the JSON document does (truncated)"
		}
		return "the response body is not JSON"
	}
	if _, err := dec.Token(); err != io.EOF {
		return "the response body has data after the JSON document"
	}
	return ""
}

func (c *Client) judgeDefault(p *prepared, raw *rawResponse) (*Response, *Error) {
	if p.expects(raw.status) {
		return c.success(p, raw)
	}
	return nil, c.errorFor(p, raw)
}

func (c *Client) contractAt(p *prepared, raw *rawResponse, msg string) *Error {
	return &Error{Kind: Contract, Module: c.module, Route: p.route, Status: raw.status, RequestID: headerRequestID(raw.header, c.token.Reveal()), Message: msg, Sent: true}
}

func (c *Client) responseOf(p *prepared, raw *rawResponse) *Response {
	secret := c.token.Reveal()
	id := headerRequestID(raw.header, secret)
	if id == "" {
		id = bodyRequestID(raw.body, secret)
	}
	return &Response{Status: raw.status, Header: raw.header, Body: raw.body, RequestID: id, module: c.module, route: p.route}
}

func (c *Client) success(p *prepared, raw *rawResponse) (*Response, *Error) {
	if raw.oversize {
		return nil, c.contractAt(p, raw, fmt.Sprintf("the response body is larger than the limit of %d bytes", c.opts.MaxBodyBytes))
	}
	if len(bytes.TrimSpace(raw.body)) > 0 {
		if !jsonContentType(raw.header) {
			return nil, c.contractAt(p, raw, "the response is not application/json")
		}
		if why := jsonProblem(raw.body); why != "" {
			return nil, c.contractAt(p, raw, why)
		}
	}
	return c.responseOf(p, raw), nil
}

// errorFor classifies an answer that is not a success of the route. The STATUS decides the kind; the envelope (when there is one) supplies the code, details and request id,
// and decides one thing: a 503 whose code is `paused` is Paused, any other 503 is Unavailable. A 4xx whose body claims to be a module error envelope and is not one is a Contract
// error (the module spoke its own vocabulary wrongly; a bare 404 from a proxy is still NotFound, with Enveloped false). A code or a body that disagrees with the status never
// changes the kind: it is data on the error.
func (c *Client) errorFor(p *prepared, raw *rawResponse) *Error {
	secret := c.token.Reveal()
	e := &Error{Module: c.module, Route: p.route, Status: raw.status, Sent: true}
	var st envState
	if !raw.oversize {
		var env envelope
		env, st = parseEnvelope(raw.body, secret)
		if st == envOK {
			e.Code, e.RequestID, e.Details, e.Enveloped = env.code, env.requestID, env.details, true
		}
	}
	if e.RequestID == "" {
		e.RequestID = headerRequestID(raw.header, secret)
	}
	s := raw.status
	switch {
	case s >= 200 && s < 300:
		e.Kind, e.Message = Contract, fmt.Sprintf("the route does not answer with HTTP %d", s)
	case s >= 300 && s < 400:
		e.Kind, e.Message = Contract, "the module answered with a redirect, which is never followed"
	case s >= 400 && s < 500 && s != http.StatusRequestTimeout && st == envMalformed:
		e.Kind, e.Message = Contract, "the error body claims to be the module's error envelope and is not one"
	case s == http.StatusBadRequest, s == http.StatusUnauthorized, s == http.StatusForbidden, s == http.StatusConflict, s == http.StatusGone,
		s == http.StatusRequestEntityTooLarge, s == http.StatusUnprocessableEntity:
		e.Kind, e.Message = Refused, fmt.Sprintf("the module refused the request (HTTP %d)", s)
	case s == http.StatusNotFound:
		e.Kind, e.Message = NotFound, "the module has no such resource"
	case s == http.StatusRequestTimeout:
		e.Kind, e.Message = Timeout, "the module timed out waiting for the request (HTTP 408)"
	case s == http.StatusTooManyRequests:
		e.Kind, e.Message = RateLimited, "the module is rate limiting this caller"
		e.RetryAfter = parseRetryAfter(raw.header, time.Now())
	case s == http.StatusMethodNotAllowed:
		e.Kind, e.Message = Contract, "the module does not serve this method on this route"
	case s >= 400 && s < 500:
		e.Kind, e.Message = Contract, fmt.Sprintf("the route does not answer with HTTP %d", s)
	case s == http.StatusBadGateway, s == http.StatusGatewayTimeout:
		e.Kind, e.Message = Unavailable, fmt.Sprintf("the module cannot be reached through its gateway (HTTP %d)", s)
	case s == http.StatusServiceUnavailable:
		e.RetryAfter = parseRetryAfter(raw.header, time.Now())
		if e.Code == "paused" {
			e.Kind, e.Message = Paused, "the module is paused by an operator; the same request will succeed later, unchanged"
		} else {
			e.Kind, e.Message = Unavailable, "the module is not ready to serve this request (HTTP 503)"
		}
	default: // 500 and every other 5xx
		e.Kind, e.Message = Internal, fmt.Sprintf("the module failed on this request (HTTP %d)", s)
	}
	return e
}

// contractFrom is a Contract error about a response that was received and judged a success but whose content the typed operations (Health, Ready, Describe) refuse.
func (c *Client) contractFrom(resp *Response, key, msg string) *Error {
	return &Error{Kind: Contract, Module: c.module, Route: resp.route, Status: resp.Status, RequestID: resp.RequestID, Key: key, Message: msg, Sent: true, Attempts: resp.Attempts}
}
