package endpoint

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/coldmail-platform/platform/internal/httpapi"
	"github.com/coldmail-platform/platform/internal/jobs"
	"github.com/coldmail-platform/platform/internal/logging"
	"github.com/coldmail-platform/platform/internal/suppress"
	"github.com/coldmail-platform/platform/internal/unsubscribe"
)

// The endpoint is tested through the whole HTTP stack of the service (httpapi.NewHandler with the endpoint mounted: the request id, the access log, the recoverer) over an in-memory fake of the two store
// calls it makes (ItemOwner and the suppression writer). Every test has a paired control: what is true of the wrong thing is shown to be false of the right one, and the reverse. The secret is built at
// run time from a repeated word (a quoted long literal next to the word "secret" is what a secret scanner flags).

const (
	item1 = "0f6c1a52-9b7e-4d1c-8a35-2e7b94c0d1aa"
	item2 = "f1e2d3c4-b5a6-4789-9abc-def012345678"
	item3 = "5a5a5a5a-1b1b-4c1c-8d1d-6e6e6e6e6e6e"
	// an item the signer will sign but the store has no row for
	itemGone = "99999999-9999-4999-8999-999999999999"

	user1 = "11111111-1111-4111-8111-111111111111"
	user2 = "22222222-2222-4222-8222-222222222222"

	addr1 = "recipient.one@example.test"
	addr2 = "recipient.two@example.test"
	addr3 = "recipient.three@example.test"

	remoteA = "192.0.2.10:40000"
	remoteB = "192.0.2.11:40000"
)

func word(w string, n int) []byte { return bytes.Repeat([]byte(w), n/len(w)+1)[:n] }

var (
	secretA = word("alpha-", 40)
	secretB = word("bravo-", 40)
)

// ---- the fakes ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------

type fakeItems struct {
	mu     sync.Mutex
	owners map[string][2]string // item -> user, address
	calls  int
	err    error
}

func (f *fakeItems) ItemOwner(_ context.Context, id string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return "", "", f.err
	}
	o, ok := f.owners[id]
	if !ok {
		return "", "", fmt.Errorf("jobs.item owner: %w", jobs.ErrNotFound)
	}
	return o[0], o[1], nil
}

func (f *fakeItems) count() int { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }

type fakeSupp struct {
	mu   sync.Mutex
	rows map[string]bool // user|address
	adds int
	err  error
}

func (f *fakeSupp) Add(_ context.Context, user, email string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.adds++
	if f.err != nil {
		return false, f.err
	}
	k := user + "|" + email
	if f.rows[k] {
		return false, nil // ON CONFLICT DO NOTHING
	}
	f.rows[k] = true
	return true, nil
}

func (f *fakeSupp) snapshot() (adds, rows int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.adds, len(f.rows)
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) advance(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.t = c.t.Add(d) }

type okPinger struct{}

func (okPinger) Ping(context.Context) error { return nil }

type stack struct {
	t      *testing.T
	h      http.Handler // the whole service: request id, access log, recoverer, the endpoint mounted
	ep     *Handler
	signer *unsubscribe.Signer
	items  *fakeItems
	supp   *fakeSupp
	logs   *syncBuf
	clock  *clock
}

func newStack(t *testing.T, mut ...func(*Config)) *stack {
	t.Helper()
	s := &stack{
		t:     t,
		items: &fakeItems{owners: map[string][2]string{item1: {user1, addr1}, item2: {user1, addr2}, item3: {user2, addr3}}},
		supp:  &fakeSupp{rows: map[string]bool{}},
		logs:  &syncBuf{},
		clock: &clock{t: time.Unix(1_700_000_000, 0)},
	}
	var err error
	if s.signer, err = unsubscribe.NewSigner(secretA); err != nil {
		t.Fatal(err)
	}
	log, err := logging.New("debug", s.logs)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Signer: s.signer, Items: s.items, Suppressions: s.supp, RatePerMin: 600, Logger: log, Now: s.clock.now}
	for _, m := range mut {
		m(&cfg)
	}
	if s.ep, err = New(cfg); err != nil {
		t.Fatalf("New: %v", err)
	}
	s.h = httpapi.NewHandler(httpapi.Deps{Logger: log, Pinger: okPinger{}, ReadyTimeout: time.Second, Mount: s.ep.Mount})
	return s
}

func (s *stack) token(item string) string {
	s.t.Helper()
	tok, err := s.signer.Token(item)
	if err != nil {
		s.t.Fatal(err)
	}
	return tok
}

type reply struct {
	status int
	header http.Header
	body   string
}

func (s *stack) do(method, target, ctype string, body io.Reader, remote string) reply {
	s.t.Helper()
	r := httptest.NewRequest(method, target, body)
	if ctype != "" {
		r.Header.Set("Content-Type", ctype)
	}
	r.RemoteAddr = remote
	w := httptest.NewRecorder()
	s.h.ServeHTTP(w, r)
	res := w.Result()
	b, _ := io.ReadAll(res.Body)
	return reply{status: res.StatusCode, header: res.Header, body: string(b)}
}

func (s *stack) get(tok string) reply { return s.do("GET", "/u/"+tok, "", nil, remoteA) }
func (s *stack) post(tok string) reply {
	return s.do("POST", "/u/"+tok, "application/x-www-form-urlencoded", strings.NewReader("List-Unsubscribe=One-Click"), remoteA)
}

// requireSecurityHeaders asserts the five headers every response carries, exactly.
func requireSecurityHeaders(t *testing.T, what string, h http.Header) {
	t.Helper()
	want := map[string]string{
		"Cache-Control":           "no-store",
		"X-Robots-Tag":            "noindex",
		"Referrer-Policy":         "no-referrer",
		"X-Content-Type-Options":  "nosniff",
		"Content-Security-Policy": "default-src 'none'; form-action 'self'; style-src 'unsafe-inline'",
	}
	for k, v := range want {
		if got := h.Get(k); got != v {
			t.Errorf("%s: %s = %q, want %q", what, k, got, v)
		}
	}
}

// ---- the tests ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------

func TestGet_ShowsTheConfirmPageAndChangesNothing(t *testing.T) {
	s := newStack(t)
	tok := s.token(item1)
	for i := 0; i < 3; i++ { // a prefetcher, a scanner and a person
		r := s.get(tok)
		if r.status != 200 || !strings.Contains(r.body, `<form method="post" action="`+tok+`">`) {
			t.Fatalf("GET = %d, the page has no POST form to the token: %s", r.status, r.body)
		}
		if strings.Contains(r.body, addr1) || strings.Contains(r.body, "recipient") || strings.Contains(r.body, user1) || strings.Contains(r.body, item1) {
			t.Fatalf("the confirm page names the address, the user or the item: %s", r.body)
		}
	}
	if adds, rows := s.supp.snapshot(); adds != 0 || rows != 0 {
		t.Fatalf("GET suppressed (%d writes, %d rows): a link prefetcher would unsubscribe everybody", adds, rows)
	}
	// the control: the POST of the same token DOES change it
	if r := s.post(tok); r.status != 200 {
		t.Fatalf("POST = %d", r.status)
	}
	if adds, rows := s.supp.snapshot(); adds != 1 || rows != 1 || !s.supp.rows[user1+"|"+addr1] {
		t.Fatalf("POST wrote %d times, %d rows: want the one row of user1 and the item's address", adds, rows)
	}
}

func TestPost_SuppressesOnceAndARepeatIsTheSameSuccessWithNoSecondRow(t *testing.T) {
	s := newStack(t)
	tok := s.token(item1)
	first := s.post(tok)
	if first.status != 200 || !strings.Contains(first.body, "You are unsubscribed") {
		t.Fatalf("first POST = %d %s", first.status, first.body)
	}
	for i := 0; i < 3; i++ {
		again := s.post(tok)
		if again.status != first.status || again.body != first.body {
			t.Fatalf("a repeat POST answered %d with another body", again.status)
		}
	}
	if _, rows := s.supp.snapshot(); rows != 1 {
		t.Fatalf("four POSTs left %d rows, want 1", rows)
	}
	// the log says created once and then not (an operator can tell the repeat from the first), and names the item only by its tag
	logs := s.logs.String()
	if strings.Count(logs, `"created":true`) != 1 || strings.Count(logs, `"created":false`) != 3 {
		t.Fatalf("the log does not say created once and not three times:\n%s", logs)
	}
	// the control: another item is another row, for its own user
	s.post(s.token(item3))
	if _, rows := s.supp.snapshot(); rows != 2 || !s.supp.rows[user2+"|"+addr3] {
		t.Fatalf("a second item did not write its own row: %v", s.supp.rows)
	}
}

func TestPost_TheFormTheOneClickBodyAndAnyOtherBodyAreTheSameRequest(t *testing.T) {
	s := newStack(t)
	for i, c := range []struct {
		name, ctype, body string
	}{
		{"the form", "application/x-www-form-urlencoded", ""},
		{"the one-click body", "application/x-www-form-urlencoded", "List-Unsubscribe=One-Click"},
		{"an empty body and no content type", "", ""},
		{"a plain-text body", "text/plain", "please unsubscribe me"},
		{"a JSON body", "application/json", `{"user_id":"` + user2 + `","email":"someone.else@example.test"}`},
		{"a form that names another address", "application/x-www-form-urlencoded", "email=someone.else%40example.test&user=" + user2},
		{"a body over the cap", "application/octet-stream", strings.Repeat("x", 1<<20)},
	} {
		item := []string{item1, item2, item3, item1, item2, item3, item1}[i]
		s2 := newStack(t)
		r := s2.do("POST", "/u/"+s2.token(item), c.ctype, strings.NewReader(c.body), remoteA)
		if r.status != 200 || !strings.Contains(r.body, "You are unsubscribed") {
			t.Errorf("%s: POST = %d", c.name, r.status)
			continue
		}
		// whatever the body said, the row is the item's own: its user and its address, never the body's
		owner := s2.items.owners[item]
		if adds, rows := s2.supp.snapshot(); adds != 1 || rows != 1 || !s2.supp.rows[owner[0]+"|"+owner[1]] {
			t.Errorf("%s: the suppression is %v, want only %s|%s", c.name, s2.supp.rows, owner[0], owner[1])
		}
	}
	// the control: the same bodies sent to a WRONG token change nothing
	for _, body := range []string{"", "List-Unsubscribe=One-Click"} {
		r := s.do("POST", "/u/"+strings.Repeat("A", 54), "application/x-www-form-urlencoded", strings.NewReader(body), remoteA)
		if r.status != 404 {
			t.Fatalf("a wrong token with the one-click body = %d, want 404", r.status)
		}
	}
	if adds, _ := s.supp.snapshot(); adds != 0 {
		t.Fatalf("a wrong token wrote %d times", adds)
	}
}

func TestWrongTokensAreAllTheSame404PageOnGetAndPost(t *testing.T) {
	s := newStack(t)
	good := s.token(item1)
	other, err := unsubscribe.NewSigner(secretB)
	if err != nil {
		t.Fatal(err)
	}
	foreign, _ := other.Token(item1)
	gone := s.token(itemGone) // signed by the platform, but no row

	flipped := []byte(good) // one character of the MAC altered
	flipped[30] = 'A'
	if good[30] == 'A' {
		flipped[30] = 'B'
	}
	tail := good[:53] + "B" // the last character carries two spare bits that must be zero: B (index 1) is not a canonical last character, and neither is C
	if good[53] == 'B' {
		tail = good[:53] + "C"
	}
	cases := []struct {
		name, path string
		storeAsked bool // the store is asked only about a token whose MAC verified
	}{
		{"a MAC that is wrong (one character altered)", "/u/" + string(flipped), false},
		{"a tail that is not canonical or not the MAC", "/u/" + tail, false},
		{"a token of another secret", "/u/" + foreign, false},
		{"a short token", "/u/" + good[:40], false},
		{"a long token", "/u/" + good + "AAAA", false},
		{"no token", "/u/", false},
		{"the word token", "/u/token", false},
		{"a token and a slash", "/u/" + good + "/", false},
		{"a token and a path", "/u/" + good + "/x", false},
		{"a token and a query that is not part of it", "/u/" + good[:40] + "?" + good[40:], false},
		{"a percent-encoded spelling of a good token", "/u/%" + fmt.Sprintf("%02X", good[0]) + good[1:], false},
		{"a good token in lower case", "/u/" + strings.ToLower(good), false},
		{"a padded token", "/u/" + good[:53] + "=", false},
		{"a signed token of an item with no row", "/u/" + gone, true},
	}
	var first *reply
	for _, m := range []string{"GET", "POST"} {
		for _, c := range cases {
			before := s.items.count()
			r := s.do(m, c.path, "application/x-www-form-urlencoded", strings.NewReader("List-Unsubscribe=One-Click"), remoteA)
			if r.status != 404 {
				t.Errorf("%s %s: %d, want 404", m, c.name, r.status)
				continue
			}
			if first == nil {
				first = &r
			}
			if r.body != first.body || r.header.Get("Content-Length") != first.header.Get("Content-Length") || r.header.Get("Content-Type") != first.header.Get("Content-Type") {
				t.Errorf("%s %s: the 404 differs from the others (body, length or type)", m, c.name)
			}
			requireSecurityHeaders(t, m+" "+c.name, r.header)
			if asked := s.items.count() - before; (asked == 1) != c.storeAsked || asked > 1 {
				t.Errorf("%s %s: the store was asked %d times, storeAsked=%v: the MAC must be judged before the lookup", m, c.name, asked, c.storeAsked)
			}
		}
	}
	if adds, _ := s.supp.snapshot(); adds != 0 {
		t.Fatalf("a wrong token suppressed %d times", adds)
	}
	// the control: the right token is not the 404 page
	if r := s.get(good); r.status != 200 || r.body == first.body {
		t.Fatalf("control failed: the good token answered %d", r.status)
	}
	if first.body == "" || strings.Contains(first.body, "form") {
		t.Fatalf("the 404 page is empty or offers a form: %q", first.body)
	}
}

func TestEveryResponseCarriesTheFiveHeaders(t *testing.T) {
	s := newStack(t)
	tok := s.token(item1)
	failing := newStack(t)
	failing.items.err = errors.New("the database is gone")
	limited := newStack(t, func(c *Config) { c.RatePerMin = 1 })
	limited.get(limited.token(item1))

	for _, c := range []struct {
		name   string
		r      reply
		status int
	}{
		{"GET 200", s.get(tok), 200},
		{"POST 200", s.post(tok), 200},
		{"404", s.get("nope"), 404},
		{"405", s.do("PUT", "/u/"+tok, "", nil, remoteA), 405},
		{"429", limited.get(limited.token(item1)), 429},
		{"500", failing.get(failing.token(item1)), 500},
		{"HEAD (a 405)", s.do("HEAD", "/u/"+tok, "", nil, remoteA), 405},
		{"OPTIONS (a 405)", s.do("OPTIONS", "/u/"+tok, "", nil, remoteA), 405},
	} {
		if c.r.status != c.status { // the loop is over the statuses it says it is over
			t.Errorf("%s: the answer is %d", c.name, c.r.status)
		}
		requireSecurityHeaders(t, c.name, c.r.header)
		if ct := c.r.header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
			t.Errorf("%s: Content-Type = %q", c.name, ct)
		}
	}
	// the control: a route of the SAME stack that is not the endpoint's does not carry them (the headers are the endpoint's doing, not the stack's)
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Header().Get("Content-Security-Policy") != "" || rec.Header().Get("Referrer-Policy") != "" {
		t.Fatal("control failed: /healthz carries the endpoint's headers, so the assertions above prove nothing")
	}
}

func TestMethodsOtherThanGetAndPostAre405WithAllow(t *testing.T) {
	s := newStack(t)
	tok := s.token(item1)
	for _, m := range []string{"PUT", "DELETE", "PATCH", "HEAD", "OPTIONS", "TRACE", "CONNECT", "PROPFIND"} {
		r := s.do(m, "/u/"+tok, "", nil, remoteA)
		if r.status != 405 || r.header.Get("Allow") != "GET, POST" {
			t.Errorf("%s: %d Allow=%q, want 405 and GET, POST", m, r.status, r.header.Get("Allow"))
		}
	}
	if s.items.count() != 0 {
		t.Fatalf("a wrong verb reached the store %d times", s.items.count())
	}
	// the controls: GET and POST are served and carry no Allow
	for _, r := range []reply{s.get(tok), s.post(tok)} {
		if r.status != 200 || r.header.Get("Allow") != "" {
			t.Fatalf("control failed: %d Allow=%q", r.status, r.header.Get("Allow"))
		}
	}
}

func TestRateLimit_AClientAddressIsHeldToItsBucketBeforeAnyWork(t *testing.T) {
	s := newStack(t, func(c *Config) { c.RatePerMin = 3 })
	tok := s.token(item1)
	for i := 0; i < 3; i++ {
		if r := s.do("GET", "/u/"+tok, "", nil, remoteA); r.status != 200 {
			t.Fatalf("request %d of 3 = %d", i+1, r.status)
		}
	}
	callsBefore := s.items.count()
	r := s.do("GET", "/u/"+tok, "", nil, remoteA)
	if r.status != 429 {
		t.Fatalf("the 4th request in a minute = %d, want 429", r.status)
	}
	ra, err := strconv.Atoi(r.header.Get("Retry-After"))
	if err != nil || ra < 1 || ra > 60 {
		t.Fatalf("Retry-After = %q, want whole seconds from 1 to 60", r.header.Get("Retry-After"))
	}
	if r.status == 429 && strings.Contains(r.body, "form") {
		t.Fatal("the 429 page offers the form")
	}
	// the limit is before the HMAC and the store: a GOOD token is 429 while limited, and the store is not asked; so is a wrong one, and a POST writes nothing
	if post := s.do("POST", "/u/"+tok, "", nil, remoteA); post.status != 429 {
		t.Fatalf("a POST while limited = %d", post.status)
	}
	if bad := s.do("GET", "/u/"+strings.Repeat("A", 54), "", nil, remoteA); bad.status != 429 {
		t.Fatalf("a wrong token while limited = %d: the 404 would tell the limited caller more than the limit does", bad.status)
	}
	if s.items.count() != callsBefore {
		t.Fatalf("the store was asked %d more times while the client was limited", s.items.count()-callsBefore)
	}
	if adds, _ := s.supp.snapshot(); adds != 0 {
		t.Fatalf("a limited POST wrote")
	}
	// another client address has its own bucket (the control), and the bucket refills evenly: 3 a minute is one every 20 seconds
	if r := s.do("GET", "/u/"+tok, "", nil, remoteB); r.status != 200 {
		t.Fatalf("another address was limited too: %d", r.status)
	}
	s.clock.advance(19 * time.Second)
	if r := s.do("GET", "/u/"+tok, "", nil, remoteA); r.status != 429 {
		t.Fatalf("19 seconds later = %d, want still 429", r.status)
	}
	s.clock.advance(2 * time.Second)
	if r := s.do("GET", "/u/"+tok, "", nil, remoteA); r.status != 200 {
		t.Fatalf("21 seconds later = %d, want one token back", r.status)
	}
	if r := s.do("GET", "/u/"+tok, "", nil, remoteA); r.status != 429 {
		t.Fatalf("a second request right after = %d, want 429 (only one token came back)", r.status)
	}
	// a bucket never holds more than its size: a long silence does not bank requests
	s.clock.advance(24 * time.Hour)
	got := 0
	for i := 0; i < 10; i++ {
		if s.do("GET", "/u/"+tok, "", nil, remoteA).status == 200 {
			got++
		}
	}
	if got != 3 {
		t.Fatalf("after a day of silence %d of 10 requests were served, want exactly the bucket's 3", got)
	}
}

func TestRateLimit_TheKeyIsTheClientAddress(t *testing.T) {
	for _, c := range []struct{ a, b string }{
		{"192.0.2.10:1", "192.0.2.10:65000"},                // the port is not part of the key
		{"[2001:db8:1:2::1]:1", "[2001:db8:1:2:ffff::9]:2"}, // an IPv6 address counts as its /64
		{"192.0.2.10:1", "[::ffff:192.0.2.10]:2"},           // an IPv4-mapped address is the IPv4 one
		{"garbage", "also garbage"},                         // an address that cannot be read shares one bucket
	} {
		if clientKey(c.a) != clientKey(c.b) {
			t.Errorf("%q and %q are %q and %q: one client", c.a, c.b, clientKey(c.a), clientKey(c.b))
		}
	}
	for _, c := range []struct{ a, b string }{
		{"192.0.2.10:1", "192.0.2.11:1"},
		{"[2001:db8:1:2::1]:1", "[2001:db8:1:3::1]:1"}, // another /64
		{"[2001:db8:1:2::1]:1", "192.0.2.10:1"},
	} {
		if clientKey(c.a) == clientKey(c.b) {
			t.Errorf("%q and %q share the key %q: two clients", c.a, c.b, clientKey(c.a))
		}
	}
}

func TestRateLimit_MemoryIsBoundedAndTheLeastRecentlySeenIsForgotten(t *testing.T) {
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	l := newLimiter(2, 50, c.now)
	for i := 0; i < 20000; i++ {
		l.allow(fmt.Sprintf("10.%d.%d.%d", i>>16&255, i>>8&255, i&255))
	}
	if n := l.size(); n != 50 {
		t.Fatalf("the limiter holds %d buckets after 20000 clients, want its bound of 50", n)
	}
	// LRU: a key used again is kept while the one used longest ago goes
	l = newLimiter(2, 3, c.now)
	l.allow("a")
	l.allow("b")
	l.allow("c")
	l.allow("a") // a is now the most recent; b the oldest
	l.allow("d") // forgets b
	for _, k := range []string{"a", "c", "d"} {
		if _, ok := l.byKey[k]; !ok {
			t.Errorf("%s was forgotten", k)
		}
	}
	if _, ok := l.byKey["b"]; ok {
		t.Error("b, the least recently seen, was kept")
	}
	// the control: below its bound the limiter forgets nobody
	l = newLimiter(2, 10000, c.now)
	for i := 0; i < 50; i++ {
		l.allow(fmt.Sprint(i))
	}
	if l.size() != 50 {
		t.Fatalf("a limiter far below its bound holds %d of 50 clients", l.size())
	}
	// and a forgotten client starts with a full bucket: that is the whole cost of the bound
	l = newLimiter(1, 1, c.now)
	l.allow("x")
	if ok, _ := l.allow("x"); ok {
		t.Fatal("a second request in the same instant was allowed")
	}
	l.allow("y") // forgets x
	if ok, _ := l.allow("x"); !ok {
		t.Fatal("a forgotten client did not start with a full bucket")
	}
}

func TestLogs_NeverHoldTheTokenTheAddressTheUserOrTheItem(t *testing.T) {
	s := newStack(t, func(c *Config) { c.RatePerMin = 4 })
	tok1, tok2 := s.token(item1), s.token(item2)
	s.get(tok1)
	s.post(tok1)
	s.post(tok1)
	s.get(strings.Repeat("A", 54))
	s.get(s.token(itemGone))
	s.do("PUT", "/u/"+tok2, "", nil, remoteB)
	s.get(tok2)
	s.get(tok2)
	s.get(tok2)
	s.get(tok2) // the limit
	failing := newStack(t)
	failing.items.err = errors.New("driver text with " + addr1 + " in it")
	failing.get(failing.token(item1))
	failing.items.err = nil
	failing.supp.err = errors.New("driver text with " + addr2)
	failing.post(failing.token(item2))

	for name, st := range map[string]*stack{"the mixed run": s, "the failing run": failing} {
		logs := st.logs.String()
		if strings.Count(logs, "\n") < 3 {
			t.Fatalf("%s: the log has too few lines to prove anything:\n%s", name, logs)
		}
		for what, secret := range map[string]string{
			"a token (item 1)": tok1, "a token (item 2)": tok2, "the first 20 characters of a token": tok1[:20], "an address": addr1, "another address": addr2, "the local part": "recipient.one",
			"a user id": user1, "an item id": item1, "another item id": item2, "the store's error text": "driver text",
		} {
			if strings.Contains(logs, secret) {
				t.Errorf("%s: the log holds %s:\n%s", name, what, logs)
			}
		}
		if !strings.Contains(logs, `"/u/[token]"`) && name == "the mixed run" {
			t.Errorf("%s: the access log does not name the path as /u/[token]:\n%s", name, logs)
		}
	}
	if !regexp.MustCompile(`"item":"[0-9a-f]{12}"`).MatchString(s.logs.String()) {
		t.Errorf("the item is not named by its 12-character tag in the log:\n%s", s.logs.String())
	}
	// the control: a naive access log of the same requests WOULD hold the token, so the search above can fail
	var naive bytes.Buffer
	fmt.Fprintf(&naive, "GET /u/%s %s %s\n", tok1, addr1, user1)
	for _, secret := range []string{tok1, addr1, user1} {
		if !strings.Contains(naive.String(), secret) {
			t.Fatal("control failed: the search does not find what a naive log holds")
		}
	}
}

func TestAServerFailureIsA500PageWithNoDetail(t *testing.T) {
	const canary = "CANARY-driver-9917"
	for name, mut := range map[string]func(*stack){
		"the lookup fails":       func(s *stack) { s.items.err = errors.New(canary + " " + addr1) },
		"the lookup times out":   func(s *stack) { s.items.err = fmt.Errorf("jobs.item owner: %w", context.DeadlineExceeded) },
		"the write fails":        func(s *stack) { s.supp.err = errors.New(canary + " " + addr1) },
		"the address is refused": func(s *stack) { s.supp.err = fmt.Errorf("suppress.add: %w", suppress.ErrCannotStore) },
	} {
		s := newStack(t)
		mut(s)
		r := s.post(s.token(item1))
		if r.status != 500 || !strings.Contains(r.body, "Something went wrong") {
			t.Errorf("%s: %d", name, r.status)
			continue
		}
		if strings.Contains(r.body, canary) || strings.Contains(r.body, addr1) || strings.Contains(r.body, "suppress") || strings.Contains(s.logs.String(), canary) {
			t.Errorf("%s: detail reached the page or the log", name)
		}
		requireSecurityHeaders(t, name, r.header)
		if !strings.Contains(s.logs.String(), `"cause":`) {
			t.Errorf("%s: the log does not classify the cause:\n%s", name, s.logs.String())
		}
	}
	// the control: the same request with nothing failing is a 200
	s := newStack(t)
	if r := s.post(s.token(item1)); r.status != 200 {
		t.Fatalf("control failed: %d", r.status)
	}
}

func TestPagesAreStaticSelfContainedAndNameNobody(t *testing.T) {
	s := newStack(t)
	tok := s.token(item1)
	pages := map[string]string{
		"confirm": s.get(tok).body, "thanks": s.post(tok).body, "404": s.get("x").body,
		"405": s.do("PUT", "/u/"+tok, "", nil, remoteA).body, "busy": pageBusy, "failed": pageFailed,
	}
	for name, p := range pages {
		for _, bad := range []string{"http://", "https://", "//", "<script", "<img", "<link", "<iframe", "<object", "<embed", "@import", "url(", "onclick", "onload", "@", "javascript:"} {
			if strings.Contains(p, bad) {
				t.Errorf("%s page contains %q: it must stand alone", name, bad)
			}
		}
		if len(p) > 3000 || !strings.HasPrefix(p, "<!doctype html>") || !strings.Contains(p, "noindex") {
			t.Errorf("%s page is %d bytes or lacks its doctype or its robots meta", name, len(p))
		}
	}
	if strings.Count(pages["confirm"], "<form") != 1 || strings.Contains(pages["thanks"], "<form") || strings.Contains(pages["404"], "<form") {
		t.Error("only the confirm page offers the form")
	}
	// the control: the token is escaped in the form's action, so even a token that was not the strict alphabet (it never reaches here) could not break out of the attribute
	if got := confirmPage(`"><script>x</script>`); strings.Contains(got, "<script>") || !strings.Contains(got, "&#34;&gt;&lt;script&gt;") {
		t.Fatalf("control failed: the action is not escaped: %s", got)
	}
}

func TestNewRefusesAWiringThatCannotWork(t *testing.T) {
	signer, _ := unsubscribe.NewSigner(secretA)
	good := Config{Signer: signer, Items: &fakeItems{}, Suppressions: &fakeSupp{}, RatePerMin: 30}
	if _, err := New(good); err != nil {
		t.Fatalf("control failed: a good config is refused: %v", err)
	}
	for name, mut := range map[string]func(*Config){
		"no signer":        func(c *Config) { c.Signer = nil },
		"no items":         func(c *Config) { c.Items = nil },
		"no suppressions":  func(c *Config) { c.Suppressions = nil },
		"a rate of 0":      func(c *Config) { c.RatePerMin = 0 },
		"a negative rate":  func(c *Config) { c.RatePerMin = -1 },
		"a rate over 600":  func(c *Config) { c.RatePerMin = 601 },
		"a negative bound": func(c *Config) { c.MaxClients = -1 },
	} {
		c := good
		mut(&c)
		if h, err := New(c); err == nil || h != nil {
			t.Errorf("%s: New accepted it", name)
		}
	}
	for _, rate := range []int{1, 600} {
		c := good
		c.RatePerMin = rate
		if _, err := New(c); err != nil {
			t.Errorf("a rate of %d was refused: %v", rate, err)
		}
	}
}

func TestMount_TheRoutesAreTheStacksAndWithoutThemThePathIsTheCatchAll404(t *testing.T) {
	s := newStack(t)
	tok := s.token(item1)
	// the endpoint is served beneath the service's middleware: a request id on every response and the access log
	r := s.get(tok)
	if !regexp.MustCompile(`^req_[0-9a-f]{32}$`).MatchString(r.header.Get(httpapi.RequestIDHeader)) {
		t.Fatalf("no request id on the endpoint's answer: %q", r.header.Get(httpapi.RequestIDHeader))
	}
	if !strings.Contains(s.logs.String(), `"msg":"http_request"`) {
		t.Fatal("the access log has no line for the endpoint's request")
	}
	// the control: a service that did not mount it answers the JSON envelope of the catch-all, not a page, for the same path
	bare := httpapi.NewHandler(httpapi.Deps{Pinger: okPinger{}, ReadyTimeout: time.Second})
	rec := httptest.NewRecorder()
	bare.ServeHTTP(rec, httptest.NewRequest("GET", "/u/"+tok, nil))
	if rec.Code != 404 || !strings.Contains(rec.Body.String(), `"not_found"`) || rec.Header().Get("Content-Security-Policy") != "" {
		t.Fatalf("an unmounted /u/ answered %d %s", rec.Code, rec.Body.String())
	}
	// and the mounted one never answers the envelope for anything under /u/
	for _, p := range []string{"/u/", "/u/x/y/z", "/u/" + tok + "/"} {
		if r := s.do("GET", p, "", nil, remoteA); r.status != 404 || strings.Contains(r.body, `"not_found"`) || !strings.Contains(r.body, "This link is not valid") {
			t.Errorf("%s answered %d %q, want the endpoint's own 404 page", p, r.status, r.body)
		}
	}
	// the probes still answer beside it
	rec = httptest.NewRecorder()
	s.h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 {
		t.Fatalf("/healthz = %d beside the endpoint", rec.Code)
	}
}

// capExec records the statements of suppress.Add.
type capExec struct {
	calls [][]any
	tag   string
}

func (c *capExec) Exec(_ context.Context, _ string, args ...any) (pgconn.CommandTag, error) {
	c.calls = append(c.calls, args)
	return pgconn.NewCommandTag(c.tag), nil
}

func TestSuppressionsOf_WritesTheNormalisedAddressWithTheUnsubscribeSource(t *testing.T) {
	ex := &capExec{tag: "INSERT 0 1"}
	created, err := SuppressionsOf(ex).Add(context.Background(), user1, "  Recipient.One@Example.TEST ")
	if err != nil || !created {
		t.Fatalf("Add = %v, %v", created, err)
	}
	if len(ex.calls) != 1 || ex.calls[0][0] != user1 || ex.calls[0][1] != "recipient.one@example.test" || ex.calls[0][2] != suppress.SourceUnsubscribe {
		t.Fatalf("the statement's arguments are %v: want the user, the NORMALISED address and the source %q", ex.calls, suppress.SourceUnsubscribe)
	}
	if suppress.SourceUnsubscribe != "unsubscribe" {
		t.Fatalf("the source is %q", suppress.SourceUnsubscribe)
	}
	// a second add that the table ignored (ON CONFLICT DO NOTHING) is not created
	ex2 := &capExec{tag: "INSERT 0 0"}
	if created, err := SuppressionsOf(ex2).Add(context.Background(), user1, addr1); err != nil || created {
		t.Fatalf("a repeat Add = %v, %v, want not created and no error", created, err)
	}
	// an address the platform's rule refuses never reaches the table
	ex3 := &capExec{tag: "INSERT 0 1"}
	if _, err := SuppressionsOf(ex3).Add(context.Background(), user1, "not an address"); !errors.Is(err, suppress.ErrInvalidAddress) || len(ex3.calls) != 0 {
		t.Fatalf("a bad address: err %v, %d statements", err, len(ex3.calls))
	}
}

func TestConcurrentPostsOfOneLinkWriteOneRow(t *testing.T) {
	s := newStack(t)
	tok := s.token(item1)
	var wg sync.WaitGroup
	statuses := make(chan int, 64)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses <- s.post(tok).status
		}()
	}
	wg.Wait()
	close(statuses)
	for st := range statuses {
		if st != 200 {
			t.Fatalf("a concurrent POST answered %d", st)
		}
	}
	if adds, rows := s.supp.snapshot(); adds != 64 || rows != 1 {
		t.Fatalf("64 POSTs made %d writes and %d rows, want 64 and 1", adds, rows)
	}
	if n := strings.Count(s.logs.String(), `"created":true`); n != 1 {
		t.Fatalf("the log says created %d times, want once", n)
	}
}

func TestDocs_UnsubscribeMDMatchesTheRoutesTheHeadersAndTheDials(t *testing.T) {
	b, err := os.ReadFile("../../../Docs/unsubscribe.md")
	if err != nil {
		t.Fatalf("cannot read Docs/unsubscribe.md (a test that cannot run must fail): %v", err)
	}
	doc := strings.ReplaceAll(string(b), "\r\n", "\n")
	drift := func(doc string) []string {
		var out []string
		rows := map[string]bool{}
		for _, m := range regexp.MustCompile("(?m)^\\| `((?:GET|POST) /u/[^`]+)` \\|").FindAllStringSubmatch(doc, -1) {
			rows[m[1]] = true
		}
		if len(rows) == 0 {
			return []string{"the route table has no rows"}
		}
		for _, r := range Routes() {
			if !rows[r] {
				out = append(out, "route "+r+" is not in the document's table")
			}
			delete(rows, r)
		}
		for r := range rows {
			out = append(out, "the document lists "+r+", which is not a route")
		}
		for _, need := range []string{
			"PLATFORM_UNSUBSCRIBE_SECRET", "PLATFORM_PUBLIC_BASE_URL", "PLATFORM_UNSUBSCRIBE_RATE_PER_MIN", "List-Unsubscribe=One-Click", "RFC 8058",
			"Cache-Control: no-store", "X-Robots-Tag: noindex", "Referrer-Policy: no-referrer", "X-Content-Type-Options: nosniff", "default-src 'none'; form-action 'self'; style-src 'unsafe-inline'",
			"base64url", "unsub|v1|", "54 characters", "Retry-After", "unsubscribe_not_configured", "forwarded-for", "429", "405", "/64",
		} {
			if !strings.Contains(doc, need) {
				out = append(out, "the document does not mention "+need)
			}
		}
		return out
	}
	if d := drift(doc); len(d) != 0 {
		t.Fatalf("Docs/unsubscribe.md and the endpoint disagree:\n  %s", strings.Join(d, "\n  "))
	}
	// the controls: a document corrupted in one way each is reported for exactly that way
	row := regexp.MustCompile("(?m)^\\| `POST /u/\\{token\\}` \\|.*\n").FindString(doc)
	if row == "" {
		t.Fatal("the POST row the control corrupts is not in the document")
	}
	for name, c := range map[string]struct{ doc, want string }{
		"a route missing":         {strings.Replace(doc, row, "", 1), "route POST /u/{token} is not in the document's table"},
		"a dial not named":        {strings.ReplaceAll(doc, "PLATFORM_UNSUBSCRIBE_RATE_PER_MIN", "PLATFORM_X"), "the document does not mention PLATFORM_UNSUBSCRIBE_RATE_PER_MIN"},
		"a route that is not one": {strings.Replace(doc, row, row+"| `GET /u/{token}/x` | x |\n", 1), "the document lists GET /u/{token}/x, which is not a route"},
	} {
		found := false
		for _, line := range drift(c.doc) {
			found = found || line == c.want
		}
		if !found {
			t.Errorf("%s: the drift report is %v, want it to say %q", name, drift(c.doc), c.want)
		}
	}
}

// TestOverARealServer runs the same stack behind a real listener and a real client: the remote address is a real one, the body deadline is set on a real connection, a one-click POST is read as a
// mail client sends it, and a path the mux itself rewrites (a doubled slash) is a redirect to the clean path and never a different token.
func TestOverARealServer_APostIsReadOnARealConnectionAndTheMuxNeverInventsAToken(t *testing.T) {
	s := newStack(t)
	srv := httptest.NewServer(s.h)
	t.Cleanup(srv.Close)
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	tok := s.token(item1)

	resp, err := client.Get(srv.URL + "/u/" + tok)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), "<form") {
		t.Fatalf("GET over the wire = %d", resp.StatusCode)
	}
	requireSecurityHeaders(t, "GET over the wire", resp.Header)

	// the one-click POST exactly as RFC 8058 gives it
	resp, err = client.Post(srv.URL+"/u/"+tok, "application/x-www-form-urlencoded", strings.NewReader("List-Unsubscribe=One-Click"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), "You are unsubscribed") {
		t.Fatalf("POST over the wire = %d %s", resp.StatusCode, b)
	}
	if _, rows := s.supp.snapshot(); rows != 1 {
		t.Fatalf("the POST left %d rows", rows)
	}
	// a body far over the cap is still answered, and still the same request
	resp, err = client.Post(srv.URL+"/u/"+s.token(item2), "text/plain", strings.NewReader(strings.Repeat("x", 4<<20)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("a POST with a 4 MiB body = %d", resp.StatusCode)
	}
	// a doubled slash is the mux's redirect to the clean path (a redirect to the SAME token, never another one) and the no-token paths are the endpoint's own 404
	resp, err = client.Get(srv.URL + "/u//" + tok)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect || resp.Header.Get("Location") != "/u/"+tok {
		t.Fatalf("a doubled slash = %d Location %q, want the redirect to the clean path", resp.StatusCode, resp.Header.Get("Location"))
	}
	for _, p := range []string{"/u/", "/u/not-a-token"} {
		resp, err = client.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		b, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 404 || !strings.Contains(string(b), "This link is not valid") {
			t.Fatalf("GET %s = %d", p, resp.StatusCode)
		}
	}
}
