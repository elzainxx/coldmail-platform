package m1results

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

	"github.com/coldmail-platform/platform/internal/config"
)

// The routes of module 1 this client calls, exactly as its api-contract.md names them. Error texts name these templates, never the URL that was
// requested (which carries the job id) and never anything from a response.
const (
	routeSubmit  = "POST /v1/verify/bulk"
	routeJob     = "GET /v1/jobs/{id}"
	routeResults = "GET /v1/jobs/{id}/results"
)

const (
	// MaxBodyBytes is the most of a response body that is read: a page of the largest allowed size (1000 results of about a kilobyte each) fits. A longer body
	// is never trusted (ErrBodyTooLarge).
	MaxBodyBytes = 8 << 20
	// DefaultCallTimeout bounds a call whose context carries no deadline of its own.
	DefaultCallTimeout = 30 * time.Second
	// DefaultPageLimit is the page size Drain asks for when Client.PageLimit is 0 (module 1's own default).
	DefaultPageLimit = 100
	// MaxPageLimit is module 1's largest page; it falls back silently to its default for anything outside 1..1000, so this client refuses what it would
	// silently change.
	MaxPageLimit = 1000
	// DefaultPoll is how long Drain waits before it looks again when the list is exhausted but the job is not complete.
	DefaultPoll = time.Second
)

var (
	// ErrInvalid: an argument was refused before any request was made.
	ErrInvalid = errors.New("m1results: the request is invalid")
	// ErrBadResponse: module 1 answered with the expected status but a body that is not the documented shape.
	ErrBadResponse = errors.New("m1results: the response is not the documented shape")
	// ErrBodyTooLarge: a response body was longer than MaxBodyBytes.
	ErrBodyTooLarge = errors.New("m1results: the response body is larger than the limit")
	// ErrNoDeadline: Drain polls until a job completes, so it refuses a context that carries no deadline (it could wait for ever).
	ErrNoDeadline = errors.New("m1results: Drain needs a context with a deadline")
	// ErrCursorStuck: a page named, as the next position, the position it was asked from.
	ErrCursorStuck = errors.New("m1results: the next cursor is the cursor that was sent: the listing does not advance")
	// ErrIncomplete: the job reports complete, but the rows read do not add up to the job's own total.
	ErrIncomplete = errors.New("m1results: the job is complete but the results read do not match its total")
)

// StatusError is a response whose status is not the one the route answers with. Code is module 1's error.code when the body is its error envelope and
// the code is a short lower-case word; nothing else of the body is kept.
type StatusError struct {
	Route  string
	Status int
	Want   int
	Code   string
}

func (e *StatusError) Error() string {
	s := fmt.Sprintf("m1results: %s answered HTTP %d, want %d", e.Route, e.Status, e.Want)
	if e.Code != "" {
		s += " (" + e.Code + ")"
	}
	return s
}

// TransportError is a request that produced no usable response: a network failure, a timeout, a body that could not be read. Reason is a class written
// here ("timeout", "connection refused"), never the error's own text, which can carry addresses and paths. Cause is reachable with errors.Is and
// errors.As (context.DeadlineExceeded works) and is never printed.
type TransportError struct {
	Route  string
	Reason string
	Cause  error
}

func (e *TransportError) Error() string {
	return "m1results: " + e.Route + ": the request failed: " + e.Reason
}

func (e *TransportError) Unwrap() error { return e.Cause }

// Item is one row of a bulk submission: the caller's own label and the address.
type Item struct {
	ItemID  string `json:"item_id"`
	Address string `json:"address"`
}

// SubmitCounts is the 202's counts over INPUT ROWS: Total = Enqueued + Duplicate + Malformed.
type SubmitCounts struct {
	Total     int
	Enqueued  int
	Duplicate int
	Malformed int
}

// JobCounts is GET /v1/jobs/{id}'s counts over input rows: Pending = Total - Settled.
type JobCounts struct {
	Total   int
	Settled int
	Pending int
}

// JobStatus is GET /v1/jobs/{id}. Complete is module 1's own judgement (decided from its work items, never from the counts).
type JobStatus struct {
	JobID            string
	Status           string
	Complete         bool
	Counts           JobCounts
	ProcessingPaused bool
}

// Client talks to one module 1 control plane with a per-caller key. Build it with New. PageLimit and Poll may be set once, before the first call; the
// rest is fixed by New.
type Client struct {
	// PageLimit is the page size Drain asks for, 1..MaxPageLimit; 0 means DefaultPageLimit.
	PageLimit int
	// Poll is how long Drain waits when the list is exhausted and the job is not complete; 0 means DefaultPoll.
	Poll time.Duration

	base string
	key  config.Secret
	hc   *http.Client
}

// New builds a Client. baseURL must be http or https with a host and carry no credentials, query or fragment (a credential in a URL is what ends up in
// a log line, so it is refused, not carried). key is the per-caller bearer key. hc may be nil, in which case the client has a 30 s timeout and no proxy; a
// supplied client is copied and its CheckRedirect replaced, so no redirect is ever followed (a 307 would re-send the key and the body to wherever it
// points). Errors from New never contain the URL or the key.
func New(baseURL string, key config.Secret, hc *http.Client) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.Opaque != "" ||
		u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, errors.New("m1results: the base URL must be an http(s) URL with a host and no credentials, query or fragment")
	}
	k := key.Reveal()
	if k == "" {
		return nil, errors.New("m1results: the key is empty")
	}
	for i := 0; i < len(k); i++ {
		if k[i] <= 0x20 || k[i] >= 0x7f {
			return nil, errors.New("m1results: the key contains whitespace, a control character or a non-ASCII character (a trailing newline from a secrets file?)")
		}
	}
	var cp http.Client
	if hc == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.Proxy = nil // a per-caller key must not be handed to a proxy
		cp = http.Client{Timeout: DefaultCallTimeout, Transport: tr}
	} else {
		cp = *hc
	}
	cp.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{base: u.Scheme + "://" + u.Host + strings.TrimRight(u.EscapedPath(), "/"), key: key, hc: &cp}, nil
}

// Format renders the Client without its key for EVERY verb (a struct field of type config.Secret is not enough on its own: fmt does not call methods on
// unexported fields, so it would print the raw string).
func (c Client) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, c.String()) }

func (c Client) String() string { return "m1results.Client{base: " + c.base + ", key: [redacted]}" }

func (c Client) GoString() string { return c.String() }

// Submit posts a bulk job in the `items` shape (each row with the caller's own item_id) and returns module 1's job id and the 202's counts. The platform
// always uses this shape: it is what makes every result attributable to exactly one input row.
func (c *Client) Submit(ctx context.Context, items []Item) (string, SubmitCounts, error) {
	body, err := json.Marshal(struct {
		Items []Item `json:"items"`
	}{items})
	if err != nil {
		return "", SubmitCounts{}, fmt.Errorf("%w: %s: the items could not be encoded", ErrInvalid, routeSubmit)
	}
	var w struct {
		JobID  string `json:"job_id"`
		Counts *struct {
			Total     int `json:"total"`
			Enqueued  int `json:"enqueued"`
			Duplicate int `json:"duplicate"`
			Malformed int `json:"malformed"`
		} `json:"counts"`
	}
	if err := c.do(ctx, routeSubmit, http.MethodPost, "/v1/verify/bulk", nil, body, http.StatusAccepted, &w); err != nil {
		return "", SubmitCounts{}, err
	}
	if w.JobID == "" || w.Counts == nil {
		return "", SubmitCounts{}, fmt.Errorf("%w: %s: no job_id or no counts", ErrBadResponse, routeSubmit)
	}
	return w.JobID, SubmitCounts{Total: w.Counts.Total, Enqueued: w.Counts.Enqueued, Duplicate: w.Counts.Duplicate, Malformed: w.Counts.Malformed}, nil
}

// Job reads the job's status and counts.
func (c *Client) Job(ctx context.Context, id string) (JobStatus, error) {
	if err := checkJobID(id); err != nil {
		return JobStatus{}, err
	}
	var w struct {
		JobID    string `json:"job_id"`
		Status   string `json:"status"`
		Complete *bool  `json:"complete"`
		Counts   *struct {
			Total   *int `json:"total"`
			Settled *int `json:"settled"`
			Pending *int `json:"pending"`
		} `json:"counts"`
		ProcessingPaused bool `json:"processing_paused"`
	}
	if err := c.do(ctx, routeJob, http.MethodGet, "/v1/jobs/"+url.PathEscape(id), nil, nil, http.StatusOK, &w); err != nil {
		return JobStatus{}, err
	}
	if w.Complete == nil || w.Counts == nil || w.Counts.Total == nil || w.Counts.Settled == nil || w.Counts.Pending == nil {
		return JobStatus{}, fmt.Errorf("%w: %s: no complete flag or no counts", ErrBadResponse, routeJob)
	}
	if w.JobID != "" && w.JobID != id {
		return JobStatus{}, fmt.Errorf("%w: %s: the answer is about another job", ErrBadResponse, routeJob)
	}
	return JobStatus{
		JobID: id, Status: w.Status, Complete: *w.Complete, ProcessingPaused: w.ProcessingPaused,
		Counts: JobCounts{Total: *w.Counts.Total, Settled: *w.Counts.Settled, Pending: *w.Counts.Pending},
	}, nil
}

// Results reads one page of the job's results, in ordinal order. cursor is "" for the first page, else the next returned by the previous page, passed back
// unchanged (it is opaque). limit is 1..MaxPageLimit, or 0 for module 1's default; anything else is refused here, because module 1 would silently fall
// back to its default. next is "" when the list is exhausted AS IT STANDS: a job that is still running can settle more rows later (see Drain).
func (c *Client) Results(ctx context.Context, id, cursor string, limit int) (rows []Row, next string, err error) {
	if err := checkJobID(id); err != nil {
		return nil, "", err
	}
	if limit < 0 || limit > MaxPageLimit {
		return nil, "", fmt.Errorf("%w: %s: the limit must be 0 (the default) or 1..%d", ErrInvalid, routeResults, MaxPageLimit)
	}
	q := url.Values{}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var w struct {
		JobID      string `json:"job_id"`
		Results    *[]Row `json:"results"`
		NextCursor string `json:"next_cursor"`
	}
	if err := c.do(ctx, routeResults, http.MethodGet, "/v1/jobs/"+url.PathEscape(id)+"/results", q, nil, http.StatusOK, &w); err != nil {
		return nil, "", err
	}
	if w.Results == nil {
		return nil, "", fmt.Errorf("%w: %s: no results array", ErrBadResponse, routeResults)
	}
	if w.JobID != "" && w.JobID != id {
		return nil, "", fmt.Errorf("%w: %s: the page is about another job", ErrBadResponse, routeResults)
	}
	return *w.Results, w.NextCursor, nil
}

func checkJobID(id string) error {
	if strings.TrimSpace(id) == "" || len(id) > 200 {
		return fmt.Errorf("%w: the job id is empty or too long", ErrInvalid)
	}
	return nil
}

// ---- Drain ------------------------------------------------------------------------------------------------------------

// DrainStats says what a Drain did.
type DrainStats struct {
	// Pages is the number of non-empty pages handed to the callback; Rows the number of rows in them.
	Pages int
	Rows  int
	// Waits is how often Drain found the list exhausted while the job was still running and waited.
	Waits int
	// CatchUp is the number of rows delivered by the pass after completion that the streaming pass had not seen: rows that settled behind the cursor.
	CatchUp int
}

// Drain reads a job's results page by page and calls fn with each page's NEW rows as it arrives, so the caller settles per page, per item, and never has
// to wait for the whole job. It returns when the job is complete and every row it has has been delivered exactly once.
//
// Why it is more than a loop over next_cursor. Module 1 lists a row once its work item has settled, in ordinal order, and a cursor is a position: a row
// that settles at a position the cursor already passed is not revisited ("read the results after complete is true"). So Drain
//
//  1. follows next_cursor while there is one, handing fn the rows it has not delivered before (an ordinal is delivered once, whatever the page it appears on);
//  2. when the list is exhausted, asks the job: not complete means wait Poll and look again from the SAME cursor (the first position it has not finished);
//  3. complete means the listing is immutable: if the rows delivered are not the job's own total, one full walk from the start delivers whatever settled
//     behind the cursor; and if they still are not, ErrIncomplete: the platform never reports a job it did not read in full.
//
// A callback error stops the drain and is returned unchanged. The context must carry a deadline (ErrNoDeadline otherwise); when it expires the error says
// what module 1 last reported, so a stuck job explains itself.
func (c *Client) Drain(ctx context.Context, jobID string, fn func(rows []Row) error) (DrainStats, error) {
	var st DrainStats
	if fn == nil {
		return st, fmt.Errorf("%w: Drain needs a callback", ErrInvalid)
	}
	if _, ok := ctx.Deadline(); !ok {
		return st, ErrNoDeadline
	}
	limit := c.PageLimit
	if limit == 0 {
		limit = DefaultPageLimit
	}
	poll := c.Poll
	if poll <= 0 {
		poll = DefaultPoll
	}
	seen := map[int]struct{}{}
	deliver := func(rows []Row) error {
		var fresh []Row
		for _, r := range rows {
			if _, dup := seen[r.Ordinal]; dup {
				continue
			}
			seen[r.Ordinal] = struct{}{}
			fresh = append(fresh, r)
		}
		if len(fresh) == 0 {
			return nil
		}
		st.Pages++
		st.Rows += len(fresh)
		return fn(fresh)
	}

	// A drain that runs out of time must say what module 1 last reported, so a stuck job explains itself (the error still unwraps to the cause).
	var last JobStatus
	haveLast := false
	explain := func(err error) error {
		if !haveLast || ctx.Err() == nil {
			return err
		}
		return fmt.Errorf("%w (module 1 last said status %q, total %d, settled %d, pending %d; %d row(s) delivered)",
			err, last.Status, last.Counts.Total, last.Counts.Settled, last.Counts.Pending, len(seen))
	}
	cursor := ""
	for {
		rows, next, err := c.Results(ctx, jobID, cursor, limit)
		if err != nil {
			return st, explain(err)
		}
		if err := deliver(rows); err != nil {
			return st, err
		}
		if next != "" {
			if next == cursor {
				return st, fmt.Errorf("%w (%d row(s) delivered)", ErrCursorStuck, st.Rows)
			}
			cursor = next
			continue
		}
		// the list is exhausted as it stands now
		job, err := c.Job(ctx, jobID)
		if err != nil {
			return st, explain(err)
		}
		last, haveLast = job, true
		if job.Complete {
			if len(seen) != job.Counts.Total {
				before := st.Rows
				if err := c.walk(ctx, jobID, limit, deliver); err != nil {
					return st, explain(err)
				}
				st.CatchUp = st.Rows - before
			}
			if len(seen) != job.Counts.Total {
				return st, fmt.Errorf("%w: %d row(s) read, the job says total %d, settled %d, pending %d", ErrIncomplete, len(seen), job.Counts.Total, job.Counts.Settled, job.Counts.Pending)
			}
			return st, nil
		}
		st.Waits++
		t := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			t.Stop()
			return st, explain(fmt.Errorf("m1results: waiting for the job to complete: %w", ctx.Err()))
		case <-t.C:
		}
	}
}

// walk reads the whole listing from the first position and hands every page to deliver.
func (c *Client) walk(ctx context.Context, jobID string, limit int, deliver func([]Row) error) error {
	cursor := ""
	for {
		rows, next, err := c.Results(ctx, jobID, cursor, limit)
		if err != nil {
			return err
		}
		if err := deliver(rows); err != nil {
			return err
		}
		if next == "" {
			return nil
		}
		if next == cursor {
			return ErrCursorStuck
		}
		cursor = next
	}
}

// ---- one request ------------------------------------------------------------------------------------------------------

// do makes one request and decodes the one JSON value of the expected answer into out. Every call has a deadline: the context's own, or
// DefaultCallTimeout when it has none.
func (c *Client) do(ctx context.Context, route, method, path string, q url.Values, body []byte, want int, out any) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultCallTimeout)
		defer cancel()
	}
	target := c.base + path
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rdr)
	if err != nil {
		return fmt.Errorf("%w: %s: the request could not be built", ErrInvalid, route)
	}
	req.Header.Set("Authorization", "Bearer "+c.key.Reveal())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "platform-m1results")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return &TransportError{Route: route, Reason: classify(err), Cause: stripURL(err)}
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, MaxBodyBytes+1))
	if err != nil {
		return &TransportError{Route: route, Reason: "reading the response body failed: " + classify(err), Cause: err}
	}
	oversized := len(b) > MaxBodyBytes
	if resp.StatusCode != want {
		return &StatusError{Route: route, Status: resp.StatusCode, Want: want, Code: errorCode(b, oversized)}
	}
	if oversized {
		return fmt.Errorf("%w: %s", ErrBodyTooLarge, route)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(out); err != nil {
		if errors.Is(err, ErrBadRow) {
			return fmt.Errorf("%w: %s: %s", ErrBadResponse, route, err.Error()) // our own words: ordinals and fixed phrases only
		}
		return fmt.Errorf("%w: %s: the body is not the documented JSON", ErrBadResponse, route)
	}
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("%w: %s: the body has data after the JSON value", ErrBadResponse, route)
	}
	return nil
}

// errorCode reads error.code from module 1's error envelope and keeps it only if it is a short lower-case word (its vocabulary is closed and additive):
// a response that stuffs anything else in the field gets nothing of it into an error string.
func errorCode(b []byte, oversized bool) string {
	if oversized {
		return ""
	}
	var e struct {
		Error *struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &e) != nil || e.Error == nil {
		return ""
	}
	code := e.Error.Code
	if code == "" || len(code) > 40 {
		return ""
	}
	for i := 0; i < len(code); i++ {
		if c := code[i]; !(c >= 'a' && c <= 'z' || c == '_') {
			return ""
		}
	}
	return code
}

// ---- transport errors -------------------------------------------------------------------------------------------------

// stripURL drops the *url.Error wrapper, whose text carries the request URL, and keeps the error beneath it (errors.Is(err, context.DeadlineExceeded)
// and net.Error still work).
func stripURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err
	}
	return err
}

// classify names the class of a transport error. It never returns err.Error(): the text of a *url.Error carries the full URL and the text of other
// errors carries addresses and paths. The unclassified case names the Go type of the innermost error.
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

// Windows reports a refused or reset connection as WSAECONNREFUSED / WSAECONNRESET, which errors.Is(err, syscall.ECONNREFUSED) does not match there.
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
