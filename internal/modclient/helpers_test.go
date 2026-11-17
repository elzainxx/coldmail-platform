package modclient

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/config"
)

// ---- fixtures: the modules' own bodies --------------------------------------------------------------------------------
//
// Every body below is the module's wire format as written in the Phase 3 research (Docs/history/p3-research) from the module's code, with the placeholders of the research
// (<uuidv7>, req_<32 hex>, <rfc3339>) replaced by well-formed concrete values, and with each fixed `message` copied from the module's own table (module 1
// internal/api/errors.go:114-129; module 2 and 3 internal/platformapi/errors.go; module 3 internal/adminapi/errors.go).

const (
	m1ID = "0198f6a2-8c3e-7b1d-9a4f-3e2d1c0b9a87" // a UUIDv7, the shape module 1 mints
	m2ID = "req_0123456789abcdef0123456789abcdef" // req_ plus 32 hex, the shape modules 2 and 3 mint
)

// Module 1 (research m1-api-and-node 1.2, 1.3, 1.6).
const (
	m1NotFound         = `{"error":{"code":"not_found","message":"The requested resource was not found.","request_id":"` + m1ID + `"}}`
	m1Unauthenticated  = `{"error":{"code":"unauthenticated","message":"Missing or invalid credentials.","request_id":"` + m1ID + `"}}`
	m1BadCursor        = `{"error":{"code":"validation_failed","message":"One or more fields are invalid.","details":[{"field":"cursor","issue":"malformed"}],"request_id":"` + m1ID + `"}}`
	m1TooLarge         = `{"error":{"code":"payload_too_large","message":"The request is larger than this endpoint accepts.","details":[{"field":"items","issue":"out_of_range"}],"request_id":"` + m1ID + `"}}`
	m1NotWired         = `{"error":{"code":"unavailable","message":"The service is not ready to serve this request.","request_id":"` + m1ID + `"}}`
	m1Paused           = `{"error":{"code":"paused","message":"The service is paused by an operator.","request_id":"` + m1ID + `"}}`
	m1Internal         = `{"error":{"code":"internal_error","message":"An unexpected error occurred.","request_id":"` + m1ID + `"}}`
	m1ContractViolated = `{"error":{"code":"contract_violation","message":"An unexpected error occurred.","request_id":"` + m1ID + `"}}`
	m1WrongVerb        = `{"error":{"code":"method_not_allowed","message":"This method is not allowed on this path.","request_id":"` + m1ID + `"}}`
)

// Module 2 (research m2-api-and-worker section 1, 2.2) and module 3 (research m3-api section 2); both emit the same Domain A envelope.
const (
	m2Validation   = `{"error":{"code":"validation_failed","message":"One or more fields are invalid.","details":[{"field":"user_id","issue":"required"}],"request_id":"` + m2ID + `"}}`
	m2Unauth       = `{"error":{"code":"unauthenticated","message":"Missing or invalid platform service token.","request_id":"` + m2ID + `"}}`
	m2NotFound     = `{"error":{"code":"not_found","message":"The requested resource was not found.","details":[{"field":"job_item_id","issue":"not_found"}],"request_id":"` + m2ID + `"}}`
	m2Conflict     = `{"error":{"code":"conflict","message":"The request conflicts with the current state.","request_id":"` + m2ID + `"}}`
	m2Paused       = `{"error":{"code":"paused","message":"Dispatch is paused by an operator; retry later.","request_id":"` + m2ID + `"}}`
	m2Internal     = `{"error":{"code":"internal_error","message":"An unexpected error occurred.","request_id":"` + m2ID + `"}}`
	m3Unprocess    = `{"error":{"code":"unprocessable","message":"This account is already paused.","details":[{"field":"paused","issue":"conflict"}],"request_id":"` + m2ID + `"}}`
	m3Suspended    = `{"error":{"code":"conflict","message":"This action is not available for your account right now.","request_id":"` + m2ID + `"}}`
	m3RateLimited  = `{"error":{"code":"rate_limited","message":"Too many requests.","request_id":"` + m2ID + `"}}`
	adminForbidden = `{"error":{"code":"forbidden","message":"This action is not permitted.","request_id":"` + m2ID + `"}}`
	adminOrdering  = `{"error":{"code":"config_ordering_violation","message":"This value would break a required ordering relationship with another config key.","request_id":"` + m2ID + `"}}`
)

// Health and readiness (research m1 1.1 and 1.7, m2 2.7, m3 1).
const (
	healthOK = `{"status":"ok","at":"2026-11-17T10:00:00.123456789Z"}`

	m1Ready200 = `{"at":"2026-11-17T10:00:00Z","checks":{"db":true,"pepper":true,"queue":true},"config":"env=development listen=:8080 ladder=cheap-layers-only(Rung3) smtp=disabled fallback=off frontdoor=serving nodes=0 mutators=recommend-only","pepper":"set","ready":true,"status":"ready","supabaseKey":"unset"}`
	m1Ready503 = `{"at":"2026-11-17T10:00:00Z","checks":{"db":true,"pepper":true,"queue":false},"config":"env=development listen=:8080 ladder=cheap-layers-only(Rung3) smtp=disabled fallback=off frontdoor=serving nodes=0 mutators=recommend-only","pepper":"set","ready":false,"reasons":["queue: TM_QUEUE_ADDR_KEY is not set: the bulk and job routes answer 503 until it is"],"status":"not_ready","supabaseKey":"unset"}`

	senderReady200 = `{"status":"ready","checks":{"db":true,"singleton_lock":true},"db":true,"singleton_lock":true,"at":"2026-11-17T10:00:00.123456Z"}`
	senderReady503 = `{"status":"not_ready","checks":{"db":false,"singleton_lock":true},"db":false,"singleton_lock":true,"at":"2026-11-17T10:00:00.123456Z"}`
)

// describeM2 is research m2 section 2.6 written out (the research elides some of each vocabulary's `fields`); describeM3 is module 3's, which publishes the same core keys, six job
// states (no `cancelled`) and the extension keys the client tolerates and does not decode (research m3 section 1).
const describeM2 = `{"module":"send_managed","api_version":"v1",
"capabilities":{"single_send":true,"sequences":false,"reply_to_per_campaign":true,"open_tracking":false,"click_tracking":false,"content_variation":true},
"pricing":{"currency":"credits","model":"flat_per_recipient","models":["flat_per_recipient","flat_subscription","free"],"unit_price_credits":1,
 "tiers":[{"id":"shared","unit_price_credits":1,"description":"Global shared SMTP pool."},{"id":"dedicated","unit_price_credits":1,"description":"User-dedicated pool with shared fallback."}],"cost_fields_inert":false},
"vocabularies":{
 "job_state":{"source":"db_enum","values":["queued","assigned","sending","sent","failed","bounced","cancelled"],"fields":["report.state","report.items[].state"]},
 "pool_kind":{"source":"db_enum","values":["shared","dedicated"],"fields":["pool.kind"]},
 "error_class":{"source":"db_enum","values":["bounce_permanent","transport_4xx","auth_failure","throttle","timeout","unknown"],"fields":["report.error_class"]},
 "detail_issue":{"source":"module_constant","values":["required","malformed","out_of_range","not_found"],"fields":["error.details[].issue"]},
 "pricing_model":{"source":"module_constant","values":["flat_per_recipient","flat_subscription","free"],"fields":["pricing.model"]},
 "item_reason":{"source":"module_constant","values":["pool_exhausted","attempts_exhausted","bounce_permanent","cancelled","transport_4xx","auth_failure","throttle","timeout","unknown","no_eligible_account","waiting_for_capacity","dispatch_paused"],"fields":["report.reason"]}},
"error_codes":[{"code":"validation_failed","http_status":400},{"code":"unauthenticated","http_status":401},{"code":"not_found","http_status":404},{"code":"method_not_allowed","http_status":405},{"code":"conflict","http_status":409},{"code":"internal_error","http_status":500},{"code":"paused","http_status":503}]}`

const describeM3 = `{"module":"send_byo","api_version":"v1",
"capabilities":{"single_send":true,"sequences":false,"reply_to_per_campaign":true,"open_tracking":false,"click_tracking":false,"content_variation":true},
"pricing":{"currency":"credits","model":"flat_subscription","models":["flat_per_recipient","flat_subscription","free"],"unit_price_credits":0,
 "tiers":[{"id":"shared","unit_price_credits":0,"description":"x"}],"cost_fields_inert":true},
"extension_routes":[{"id":"accounts.list","method":"GET","path":"/v1/send-byo/accounts"}],
"schemas":{"report":{"type":"object"}},
"vocabularies":{
 "job_state":{"source":"db_enum","values":["queued","assigned","sending","sent","failed","bounced"],"fields":["report.state","report.items[].state"]},
 "error_class":{"source":"db_enum","values":["bounce_permanent","transport_4xx","auth_failure","throttle","timeout","unknown"],"fields":["report.error_class"]}},
"error_codes":[{"code":"validation_failed","http_status":400},{"code":"unauthenticated","http_status":401},{"code":"not_found","http_status":404},{"code":"method_not_allowed","http_status":405},{"code":"conflict","http_status":409},{"code":"unprocessable","http_status":422},{"code":"rate_limited","http_status":429},{"code":"internal_error","http_status":500},{"code":"paused","http_status":503}],
"bulk_add":{"warning_code":"credentials_in_plaintext_file","warning_text":"x"}}`

// ---- helpers -----------------------------------------------------------------------------------------------------------

// mustToken makes a bearer secret at run time (no secret is committed). Its alphabet (lower-case hex and '-') is the alphabet of a module's identifiers, so a module that echoes
// it into a code or an id would get it past the alphabet check and only the contains-the-secret check keeps it out of an error.
func mustToken(t testing.TB) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return "tok-" + hex.EncodeToString(b[:])
}

func ctxT(t testing.TB, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

// seenReq is one request a stub received.
type seenReq struct {
	method, path, rawQuery, auth, contentType, accept, userAgent string
	body                                                         []byte
}

// stub is an httptest server that counts its requests, records them and hands each to a handler with its 1-based attempt number.
type stub struct {
	srv     *httptest.Server
	hits    atomic.Int64
	release chan struct{} // closed at cleanup: a handler that blocks selects on it
	mu      sync.Mutex
	seen    []seenReq
}

func newStub(t testing.TB, h func(w http.ResponseWriter, r *http.Request, n int)) *stub {
	t.Helper()
	s := &stub{release: make(chan struct{})}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(s.hits.Add(1))
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		s.mu.Lock()
		s.seen = append(s.seen, seenReq{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"), r.Header.Get("Content-Type"), r.Header.Get("Accept"), r.Header.Get("User-Agent"), body})
		s.mu.Unlock()
		h(w, r, n)
	}))
	t.Cleanup(s.srv.Close)
	t.Cleanup(func() { close(s.release) }) // runs BEFORE srv.Close (cleanups are last in, first out), so a blocked handler lets the server shut down
	return s
}

func (s *stub) count() int { return int(s.hits.Load()) }

func (s *stub) requests() []seenReq {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]seenReq(nil), s.seen...)
}

// block holds the handler until the stub shuts down or the client goes away.
func (s *stub) block(r *http.Request) {
	select {
	case <-s.release:
	case <-r.Context().Done():
	}
}

// transport is a fresh transport per test (own connection pool, no proxy), closed at cleanup.
func transport(t testing.TB) *http.Client {
	t.Helper()
	tr := &http.Transport{}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr}
}

// recorder is Options.Sleep that records the requested waits and returns at once.
type recorder struct {
	mu    sync.Mutex
	waits []time.Duration
}

func (r *recorder) sleep(ctx context.Context, d time.Duration) error {
	r.mu.Lock()
	r.waits = append(r.waits, d)
	r.mu.Unlock()
	return ctx.Err()
}

func (r *recorder) got() []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Duration(nil), r.waits...)
}

// client builds a Client for module m against s with the given options (a test timeout of two seconds unless set).
func (s *stub) client(t testing.TB, m Module, token string, opts Options) *Client {
	t.Helper()
	if opts.Timeout == 0 {
		opts.Timeout = 2 * time.Second
	}
	c, err := New(m, s.srv.URL, config.Secret(token), transport(t), opts)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func jsonReply(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func getReq(route, path string) Request {
	return Request{Route: route, Method: http.MethodGet, Path: path}
}

// asErr requires err to be an *Error.
func asErr(t testing.TB, err error) *Error {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("error %v (%T) is not a *modclient.Error", err, err)
	}
	return e
}

var sentinels = map[Kind]error{
	Unavailable: ErrUnavailable, Paused: ErrPaused, Timeout: ErrTimeout, Internal: ErrInternal, Contract: ErrContract, Refused: ErrRefused,
	NotFound: ErrNotFound, RateLimited: ErrRateLimited, Canceled: ErrCanceled, Invalid: ErrInvalid, NotSupported: ErrNotSupported,
}

// wantKind requires err to be an *Error of exactly this kind: errors.As finds it, errors.Is is true for its sentinel and false for every other.
func wantKind(t testing.TB, err error, want Kind) *Error {
	t.Helper()
	if err == nil {
		t.Fatalf("no error, want kind %s", want)
	}
	e := asErr(t, err)
	if e.Kind != want {
		t.Fatalf("kind %s, want %s: %v", e.Kind, want, err)
	}
	for k, s := range sentinels {
		if got := errors.Is(err, s); got != (k == want) {
			t.Fatalf("errors.Is(err, Err%s) = %v for an error of kind %s", k, got, want)
		}
	}
	return e
}

func containsAny(text string, needles ...string) string {
	for _, n := range needles {
		if n != "" && strings.Contains(text, n) {
			return n
		}
	}
	return ""
}
