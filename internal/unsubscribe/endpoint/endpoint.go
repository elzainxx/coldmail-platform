// Package endpoint is the PUBLIC unsubscribe endpoint (Phase 5, chunk S3): the two routes a recipient's browser or mail client reaches from the link in a message.
//
//	GET  /u/{token}   a small confirm page: one button, a POST form to the same path. It CHANGES NOTHING, so a link prefetcher, a security scanner or a preview never unsubscribes anybody.
//	POST /u/{token}   the unsubscribe: the form's POST, or the RFC 8058 one-click POST a mail client sends (body List-Unsubscribe=One-Click). It adds the recipient's address to the owner's
//	                  suppression list and answers a plain thank-you page. It is idempotent: a second POST is the same success and writes no second row.
//
// WHO MAY CALL IT. Anybody: it is NOT behind the bearer middleware of internal/api, and it must not be (a recipient has no account). What a caller needs is the token, which is a capability for one
// item (internal/unsubscribe: 16 bytes of item id and 24 bytes of HMAC-SHA256 under the platform's secret), and what it can do with it is exactly one thing: suppress the one address that item was
// written for, for the one user whose job it was. Nothing in a response carries an address, a user, a job or a count.
//
// ONE ANSWER FOR EVERY WRONG TOKEN. A token of the wrong length or alphabet, a token whose MAC is wrong, a token minted under another secret, and a well-signed token of an item the platform has no row
// for are the SAME 404 page with the same status, headers and length. The MAC is always computed BEFORE the store is asked anything (so the time a malformed token takes is not shorter than a
// well-formed one's), a token is only ever compared in constant time (internal/unsubscribe), and the store's ErrNotFound is the unknown item. Nothing here enumerates.
//
// WHAT IS NEVER LOGGED. The token, the address and the owner. A line that has to name an item names a 12-character hash of the item id (itemTag), which is not reversible to the id and cannot be
// used to build a link. The access log of internal/httpapi writes the request path, which for these routes would be the token: it writes `/u/[token]` instead (a test in this package runs the whole
// handler stack and searches every log line for the token).
//
// EVERY RESPONSE, whatever its status, carries Cache-Control: no-store, X-Robots-Tag: noindex, Referrer-Policy: no-referrer, X-Content-Type-Options: nosniff and a Content-Security-Policy of
// `default-src 'none'; form-action 'self'; style-src 'unsafe-inline'`: the pages are static HTML with one inline style and no external resource, and they do not frame or script.
//
// THE RATE LIMIT is a token bucket per client address (the connection's remote address; an IPv6 address counts as its /64), applied BEFORE any HMAC or database work, with bounded memory (an LRU of
// MaxClients buckets). Behind a reverse proxy the remote address is the proxy's until a deployment phase configures a trusted forwarded-for header, so there every recipient shares one bucket;
// that is documented in Docs/unsubscribe.md and deliberately not guessed at here (a header trusted by default is a header anybody can forge). A request over the limit is 429 with Retry-After.
package endpoint

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coldmail-platform/platform/internal/jobs"
	"github.com/coldmail-platform/platform/internal/logging"
	"github.com/coldmail-platform/platform/internal/suppress"
	"github.com/coldmail-platform/platform/internal/unsubscribe"
)

// Pattern is the route pattern the handler mounts: every path under /u/ is the endpoint's (a token with a slash in it is a wrong token, answered by the same 404 page as any other).
const Pattern = "/u/{token...}"

// Routes are the two public routes, in the form of the route table of Docs/unsubscribe.md (a test holds the document to this list).
func Routes() []string { return []string{"GET /u/{token}", "POST /u/{token}"} }

// Defaults of the Config.
const (
	// DefaultMaxClients is how many client addresses the limiter remembers at once; the least recently seen is forgotten first.
	DefaultMaxClients = 10000
	// MaxBodyBytes is how much of a POST's body is read (and ignored): the form is a few bytes and the one-click body is 26.
	MaxBodyBytes = 1024
	// DefaultTimeout bounds the store calls of one request.
	DefaultTimeout = 15 * time.Second
	// BodyTimeout is how long a POST's body has to arrive.
	BodyTimeout = 15 * time.Second
)

// Items is what the endpoint needs of the job store: *jobs.Store satisfies it. An unknown item is jobs.ErrNotFound.
type Items interface {
	ItemOwner(ctx context.Context, itemID string) (userID, email string, err error)
}

var _ Items = (*jobs.Store)(nil)

// Suppressions is what the endpoint needs to suppress an address for a user: it reports whether the row was newly written (false: it was already there). SuppressionsOf is the production one.
type Suppressions interface {
	Add(ctx context.Context, userID, email string) (created bool, err error)
}

// SuppressionsOf is Suppressions over internal/suppress: Add validates the address first, stores its normalised form with the source `unsubscribe` and reports a second add of the same address as
// not created.
func SuppressionsOf(q suppress.Execer) Suppressions { return suppressionsOf{q: q} }

type suppressionsOf struct{ q suppress.Execer }

func (s suppressionsOf) Add(ctx context.Context, userID, email string) (bool, error) {
	r, err := suppress.Add(ctx, s.q, userID, email, suppress.SourceUnsubscribe)
	return r.Created, err
}

// Config is what New builds the handler from. Signer, Items and Suppressions are required; RatePerMin must be 1 to MaxRatePerMin.
type Config struct {
	Signer       *unsubscribe.Signer
	Items        Items
	Suppressions Suppressions
	// RatePerMin is how many requests one client address may make in a minute (PLATFORM_UNSUBSCRIBE_RATE_PER_MIN): the bucket holds this many and refills evenly.
	RatePerMin int
	// MaxClients is how many buckets the limiter keeps; 0 means DefaultMaxClients.
	MaxClients int
	// Timeout bounds the store calls of one request; 0 means DefaultTimeout.
	Timeout time.Duration
	Logger  *slog.Logger
	// Now is the limiter's clock; nil means time.Now (a test replaces it).
	Now func() time.Time
}

// MaxRatePerMin is the largest RatePerMin (internal/config pins PLATFORM_UNSUBSCRIBE_RATE_PER_MIN's range to the same bound).
const MaxRatePerMin = 600

// Handler serves the two routes. It is safe for concurrent use.
type Handler struct {
	signer  *unsubscribe.Signer
	items   Items
	supp    Suppressions
	limit   *limiter
	timeout time.Duration
	log     *slog.Logger
}

// New builds the handler. A missing dependency or a rate out of range is a wiring bug and an error at start-up.
func New(c Config) (*Handler, error) {
	switch {
	case c.Signer == nil:
		return nil, errors.New("endpoint.New: no signer")
	case c.Items == nil:
		return nil, errors.New("endpoint.New: no item store")
	case c.Suppressions == nil:
		return nil, errors.New("endpoint.New: no suppression writer")
	case c.RatePerMin < 1 || c.RatePerMin > MaxRatePerMin:
		return nil, fmt.Errorf("endpoint.New: the rate must be between 1 and %d requests a minute, got %d", MaxRatePerMin, c.RatePerMin)
	case c.MaxClients < 0:
		return nil, errors.New("endpoint.New: the number of remembered clients cannot be negative")
	}
	max := c.MaxClients
	if max == 0 {
		max = DefaultMaxClients
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	now := c.Now
	if now == nil {
		now = time.Now
	}
	log := c.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Handler{
		signer: c.Signer, items: c.Items, supp: c.Suppressions,
		limit:   newLimiter(c.RatePerMin, max, now),
		timeout: timeout,
		log:     logging.Contextual(log),
	}, nil
}

// Mount registers the routes on mux (httpapi.Deps.Mount). One pattern, no method: the handler answers the verbs itself so that every response, a 405 included, carries the headers.
func (h *Handler) Mount(mux *http.ServeMux) { mux.HandleFunc(Pattern, h.ServeHTTP) }

// ServeHTTP is the whole endpoint.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setHeaders(w.Header())
	switch r.Method {
	case http.MethodGet, http.MethodPost:
	default:
		w.Header().Set("Allow", "GET, POST")
		writePage(w, http.StatusMethodNotAllowed, pageMethod)
		return
	}
	// the limit comes before any HMAC or database work, and counts every GET and POST that reaches the endpoint, right or wrong
	if ok, wait := h.limit.allow(clientKey(r.RemoteAddr)); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(wait)))
		writePage(w, http.StatusTooManyRequests, pageBusy)
		return
	}
	// the token is the raw (still escaped) path after /u/: a percent-encoded spelling of a good token is NOT the token, it is a wrong one
	token := strings.TrimPrefix(r.URL.EscapedPath(), "/u/")
	itemID, ok := h.signer.Verify(token) // the MAC is computed whatever the token looks like, and before the store is asked anything
	if !ok {
		writePage(w, http.StatusNotFound, pageNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.timeout)
	defer cancel()
	userID, email, err := h.items.ItemOwner(ctx, itemID)
	switch {
	case errors.Is(err, jobs.ErrNotFound):
		writePage(w, http.StatusNotFound, pageNotFound)
		return
	case err != nil:
		h.fault(r.Context(), "item lookup failed", itemID, err)
		writePage(w, http.StatusInternalServerError, pageFailed)
		return
	}
	if r.Method == http.MethodGet {
		writePage(w, http.StatusOK, confirmPage(token))
		return
	}

	// POST: whatever the body is (the form, the one-click body, anything else) is read up to a tiny cap and ignored; the token is the whole request. The server has no read timeout of its own, so the
	// body gets BodyTimeout to arrive (a connection that sends a few bytes and stops does not hold a goroutine for ever); a writer that cannot set a deadline (a test recorder) is left as it is
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(BodyTimeout))
	_, _ = io.Copy(io.Discard, http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	created, err := h.supp.Add(ctx, userID, email)
	if err != nil {
		h.fault(r.Context(), "suppression failed", itemID, err)
		writePage(w, http.StatusInternalServerError, pageFailed)
		return
	}
	h.log.InfoContext(r.Context(), "unsubscribe recorded", "item", itemTag(itemID), "created", created)
	writePage(w, http.StatusOK, pageThanks)
}

// fault logs why a request became a 500: the step and a class of the cause, never the cause's text (a driver's text may quote a row) and never the token or the address.
func (h *Handler) fault(ctx context.Context, what, itemID string, err error) {
	h.log.ErrorContext(ctx, "unsubscribe "+what, "item", itemTag(itemID), "cause", causeOf(err))
}

// causeOf is a word for what went wrong.
func causeOf(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, suppress.ErrInvalid):
		return "address_refused_by_the_rule"
	case errors.Is(err, suppress.ErrCannotStore):
		return "address_cannot_be_stored"
	case errors.Is(err, suppress.ErrUnknownUser), errors.Is(err, suppress.ErrSystemUser):
		return "owner_cannot_hold_a_suppression"
	}
	return "database_or_other"
}

// itemTag is the only way a log line names an item: 12 hexadecimal characters of a SHA-256 over a domain text and the item id. It cannot be turned back into the id, and the id is not a link
// without the platform's secret, but the tag is kept short and one-way all the same: it lets an operator follow one request through the lines, and nothing else.
func itemTag(itemID string) string {
	sum := sha256.Sum256([]byte("unsub-log|" + itemID))
	return hex.EncodeToString(sum[:6])
}

// setHeaders is what every response carries.
func setHeaders(h http.Header) {
	h.Set("Cache-Control", "no-store")
	h.Set("X-Robots-Tag", "noindex")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; form-action 'self'; style-src 'unsafe-inline'")
}

func writePage(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// ---- the pages: static, in English, with one inline style and nothing external, and never an address, a name or a count --------------------------------------------------------------------------

const pageHead = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><meta name="robots" content="noindex, nofollow"><title>Unsubscribe</title>
<style>body{font-family:system-ui,-apple-system,"Segoe UI",sans-serif;max-width:34rem;margin:4rem auto;padding:0 1rem;line-height:1.5;color:#1d1d1f}h1{font-size:1.4rem}button{font:inherit;padding:.6rem 1.4rem;cursor:pointer}</style></head>
<body>
`

const pageFoot = `</body></html>
`

const (
	pageThanks = pageHead + `<h1>You are unsubscribed</h1>
<p>Your request was recorded. This sender will not include you in future mailings. A message that is already on its way may still arrive.</p>
` + pageFoot
	pageNotFound = pageHead + `<h1>This link is not valid</h1>
<p>The unsubscribe link could not be used. It may have been copied incompletely.</p>
` + pageFoot
	pageFailed = pageHead + `<h1>Something went wrong</h1>
<p>Your request could not be completed. Please try again in a little while.</p>
` + pageFoot
	pageBusy = pageHead + `<h1>Too many requests</h1>
<p>Please wait a moment and try again.</p>
` + pageFoot
	pageMethod = pageHead + `<h1>Not available</h1>
<p>This page can only be opened or submitted.</p>
` + pageFoot
)

// confirmPage is the page a GET shows: one button, in a form that POSTs to the same path. The form's action is the token itself, as a relative reference (the page is at /u/<token>, so the reference
// resolves to the same path wherever the service is mounted); the token is the one that was just verified (54 characters of the URL-safe alphabet), and it is escaped all the same.
func confirmPage(token string) string {
	return pageHead + `<h1>Unsubscribe</h1>
<p>Confirm that you no longer want to receive these emails. It takes effect right away.</p>
<form method="post" action="` + html.EscapeString(token) + `"><button type="submit">Unsubscribe</button></form>
` + pageFoot
}

// ---- the rate limit ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------

// clientKey is the limiter's key for a connection's remote address: the host without its port, an IPv4-mapped IPv6 address as the IPv4 one, an IPv6 address as its /64, and "unknown" when the
// address cannot be read (every such connection shares one bucket).
func clientKey(remote string) string {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	if i := strings.IndexByte(host, '%'); i >= 0 { // a zone
		host = host[:i]
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return "unknown"
	}
	ip = ip.Unmap()
	if ip.Is6() {
		p, err := ip.Prefix(64)
		if err != nil {
			return "unknown"
		}
		return p.String()
	}
	return ip.String()
}

type bucket struct {
	key    string
	tokens float64
	last   time.Time
}

// limiter is a token bucket per key: it holds up to burst tokens and refills at burst/60 a second. Its memory is bounded by max keys, an LRU: the key seen longest ago is forgotten first (a
// forgotten key starts again with a full bucket, which is the limiter's only softness, and the price of not growing without bound).
type limiter struct {
	mu    sync.Mutex
	now   func() time.Time
	burst float64
	rate  float64 // tokens per second
	max   int
	order *list.List // front: most recently seen
	byKey map[string]*list.Element
}

func newLimiter(perMin, max int, now func() time.Time) *limiter {
	return &limiter{now: now, burst: float64(perMin), rate: float64(perMin) / 60, max: max, order: list.New(), byKey: make(map[string]*list.Element)}
}

// allow takes one token from key's bucket. When there is none it says how long until there is one.
func (l *limiter) allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	var b *bucket
	if el, ok := l.byKey[key]; ok {
		l.order.MoveToFront(el)
		b = el.Value.(*bucket)
		if dt := now.Sub(b.last).Seconds(); dt > 0 {
			b.tokens = min(l.burst, b.tokens+dt*l.rate)
		}
		b.last = now
	} else {
		b = &bucket{key: key, tokens: l.burst, last: now}
		l.byKey[key] = l.order.PushFront(b)
		for len(l.byKey) > l.max {
			old := l.order.Back()
			delete(l.byKey, old.Value.(*bucket).key)
			l.order.Remove(old)
		}
	}
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
}

// size is how many buckets are held (a test reads it).
func (l *limiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.byKey)
}

// retryAfterSeconds is the wait rounded UP to whole seconds, at least one.
func retryAfterSeconds(d time.Duration) int {
	s := int((d + time.Second - 1) / time.Second)
	return max(s, 1)
}
