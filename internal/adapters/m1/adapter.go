// Package m1 is the platform's adapter for module 1 (coldmail-verify): the module's verbs mapped to platform types, and nothing else. It replaces internal/m1results, whose
// contents it carries (the item-keyed results walk and the mapping to money.SettleItem), now built on internal/modclient so that every failure is a typed modclient error.
//
// WHO THE CALLER IS. Module 1's tenant is the caller key that submits a job (internal/tenant derives one per user), so an Adapter holds ONE user's key and a Factory makes one per
// tenant.Scope. The module itself refuses a stranger's job (404), which is why the adapter has no user id to send.
//
// THE VERBS.
//
//	Prepare   (pure) normalises every address with internal/norm; a malformed address is NEVER sent: it is counted in the Plan and mapped to the free outcome of charge row P-08.
//	Submit    POST /v1/verify/bulk under a caller-supplied Idempotency-Key that EVERY submission carries, so the request is idempotent and retried per the dials with the same key; a repeat
//	          is the same job (SubmitResult.Replayed), the same key with another request is a 409 (errors.Is(err, ErrIdempotencyKeyReused)), and an outcome still unconfirmed after the
//	          retries is the typed Unavailable or Timeout error of the last attempt, which says the same key may be sent again.
//	Status    GET /v1/jobs/{id}: status, counts and totals from one snapshot, cross-checked. Idempotent, retried per the dials.
//	Results   one page of GET /v1/jobs/{id}/results; Drain walks every page and settles as rows arrive (and walks again once a job completes behind the cursor).
//	Verify    POST /v1/verify for one address, synchronously. Billed, so never retried.
//	Describe, CheckVocabularies   GET /v1/describe: the module says what it may say; the adapter checks it is module 1 offering what it relies on, and compares its vocabularies and error
//	          codes with the sets it handles, exactly as the adapter of modules 2 and 3 does.
//	Health, Ready   pass-throughs on the modclient.
//
// WHAT IT NEVER DOES. It never invents an outcome: a module that is down, slow, paused or answering an unexpected shape yields a typed *modclient.Error (modclient.KindOf), never a
// default verdict and never a zero count that looks like an answer. A function that returns an error returns no value beside it. It holds no state between calls and has no job logic
// (the platform's job engine is Phase 4).
package m1

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/modclient"
	"github.com/coldmail-platform/platform/internal/tenant"
)

const (
	// DefaultPageLimit is the page size Drain asks for when Adapter.PageLimit is 0 (module 1's own default).
	DefaultPageLimit = 100
	// MaxPageLimit is module 1's largest page.
	MaxPageLimit = 1000
	// DefaultPoll is how long Drain waits before it looks again when the list is exhausted but the job is not complete.
	DefaultPoll = time.Second

	// submitTimeoutFloor is the least time one bulk submission may take: module 1 stores up to 100,000 rows in the request, and a timeout that fires while it is still working is an
	// unconfirmed outcome that has to be repeated with the same key (safe, but a wasted attempt of a job it may well have stored). The dial (PLATFORM_MODULE_TIMEOUT_MS) applies when
	// it is larger.
	submitTimeoutFloor = 2 * time.Minute
	// verifyTimeoutFloor is the least time one synchronous verification may take: the module holds the request for its DNS lookups and the node's SMTP session (the control plane's
	// node deadline is 15 s by default and 30 s in the harness).
	verifyTimeoutFloor = 45 * time.Second
)

// newErr is a typed refusal made here (an argument refused before any request, or a contract broken by what came back).
func newErr(kind modclient.Kind, route, msg string) *modclient.Error {
	return &modclient.Error{Kind: kind, Module: modclient.M1, Route: route, Message: msg}
}

// Adapter talks to module 1 as one tenant (one caller key). Build it with New or Factory.ForScope. It is safe for concurrent use after PageLimit and Poll are set; set them once,
// before the first call.
type Adapter struct {
	// PageLimit is the page size Drain asks for, 1..MaxPageLimit; 0 means DefaultPageLimit.
	PageLimit int
	// Poll is how long Drain waits when the list is exhausted and the job is not complete; 0 means DefaultPoll.
	Poll time.Duration

	c       *modclient.Client
	timeout time.Duration // the per-attempt dial, completed with modclient's default
}

// New builds an Adapter for one caller key. baseURL, hc and opts are modclient.New's: a base URL that is not plain http(s) with a host (no credentials, query or fragment), a key that
// is empty or has whitespace or a non-ASCII character, and bad dials are refused with an Invalid error that never quotes the URL or the key; a nil hc gets a client with its own
// transport and no proxy; no redirect is ever followed.
func New(baseURL string, key config.Secret, hc *http.Client, opts modclient.Options) (*Adapter, error) {
	if key.Reveal() == "" {
		return nil, newErr(modclient.Invalid, "m1.new", "module 1 needs a caller key and none was given (a scope without a key cannot call module 1)")
	}
	c, err := modclient.New(modclient.M1, baseURL, key, hc, opts)
	if err != nil {
		return nil, err
	}
	t := opts.Timeout
	if t == 0 {
		t = modclient.DefaultTimeout
	}
	return &Adapter{c: c, timeout: t}, nil
}

// Format renders the Adapter without its key for EVERY verb (the modclient.Client inside redacts itself, and this keeps fmt from printing the pointer's fields).
func (a Adapter) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, a.String()) }

func (a Adapter) String() string { return "m1.Adapter{" + a.c.String() + "}" }

func (a Adapter) GoString() string { return a.String() }

// ---- the factory --------------------------------------------------------------------------------------------------------

// Factory makes one Adapter per tenant over one shared transport (one connection pool; the key travels per request, so nothing of one tenant's call is visible to another's).
type Factory struct {
	base string
	opts modclient.Options
	hc   *http.Client
}

// factoryProbeKey stands in for a key while NewFactory has modclient.New judge the URL and the dials, so the rules live in one place. It is not a credential and is never sent.
const factoryProbeKey = "probe-not-a-credential"

// FromConfig builds the Factory from the platform's configuration: cfg.M1URL (PLATFORM_M1_URL) and, through modclient.FromConfig, the module timeout, retries and backoff. An unset
// URL is an Invalid error naming the variable, never a Factory that fails later.
func FromConfig(cfg config.Config) (*Factory, error) {
	if cfg.M1URL == "" {
		return nil, newErr(modclient.Invalid, "m1.config", "module 1's URL is not configured ("+config.VarM1URL+" is unset), so no module 1 call can be made")
	}
	return NewFactory(cfg.M1URL, modclient.FromConfig(cfg), nil)
}

// NewFactory builds a Factory over baseURL with the given dials. hc may be nil (a client with its own transport and no proxy).
func NewFactory(baseURL string, opts modclient.Options, hc *http.Client) (*Factory, error) {
	if hc == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.Proxy = nil // a caller key must never be handed to a proxy
		hc = &http.Client{Transport: tr}
	}
	if _, err := modclient.New(modclient.M1, baseURL, config.Secret(factoryProbeKey), hc, opts); err != nil {
		return nil, err
	}
	return &Factory{base: baseURL, opts: opts, hc: hc}, nil
}

// ForScope makes the Adapter that acts for the tenant of s: it sends s.M1Key and nothing else identifies the tenant. A scope without a key is an Invalid refusal made before
// anything is sent.
func (f *Factory) ForScope(s tenant.Scope) (*Adapter, error) {
	if f == nil {
		return nil, newErr(modclient.Invalid, "m1.scope", "there is no module 1 factory")
	}
	if s.M1Key.Reveal() == "" {
		return nil, newErr(modclient.Invalid, "m1.scope", "the tenant scope carries no module 1 key (PLATFORM_M1_KEY_SECRET unset, or a Scope built by hand)")
	}
	return New(f.base, s.M1Key, f.hc, f.opts)
}

// ---- pass-throughs ------------------------------------------------------------------------------------------------------

// Health is module 1's liveness (GET /healthz).
func (a *Adapter) Health(ctx context.Context) (*modclient.Health, error) { return a.c.Health(ctx) }

// Ready is module 1's readiness (GET /readyz): its checks (db, pepper, and queue where declared) and its reasons, as modclient.Readiness. "Not ready" is an answer, not an error. It
// says nothing about the verifier node or the SMTP stage (research m1 1.7): only verdicts show that.
func (a *Adapter) Ready(ctx context.Context) (*modclient.Readiness, error) { return a.c.Ready(ctx) }

// contractErr is a Contract error about an answer that was received and judged a success but whose content the adapter refuses: key names the response key, msg is a sentence
// written here (never a value from the response).
func contractErr(route string, resp *modclient.Response, key, msg string) *modclient.Error {
	e := newErr(modclient.Contract, route, msg)
	e.Key, e.Sent = key, true
	if resp != nil {
		e.Status, e.RequestID, e.Attempts = resp.Status, resp.RequestID, resp.Attempts
	}
	return e
}

// asModclient is err as the *modclient.Error it is, or nil.
func asModclient(err error) *modclient.Error {
	var e *modclient.Error
	if errors.As(err, &e) {
		return e
	}
	return nil
}
