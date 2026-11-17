package modclient

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"
)

// The operations every module serves (or, for describe, two of the three): liveness, readiness and self-description. Each returns (value, error) and on any error the value is nil.

// ---- health ----------------------------------------------------------------------------------------------------------

// Health is a module's liveness answer: GET /healthz, 200 {"status":"ok","at":"<RFC 3339>"} on all three modules (module 1 api.go, module 2 and 3 httpapi/health.go). It is
// 200 even when every dependency of the module is down: liveness says the process answers, nothing more (Ready says whether it can work).
type Health struct {
	Status    string
	At        time.Time
	RequestID string
}

var healthShape = Object(Req("status", Str), Req("at", Str))

// Health calls GET /healthz (idempotent, no credential sent). A module that does not answer, or answers anything but status "ok" with an RFC 3339 `at`, is an error and the
// *Health is nil.
func (c *Client) Health(ctx context.Context) (*Health, error) {
	resp, e := c.do(ctx, Request{Route: string(c.module) + ".healthz", Method: http.MethodGet, Path: "/healthz", Idempotent: true, Anonymous: true}, c.judgeDefault)
	if e != nil {
		return nil, e
	}
	var w struct {
		Status string `json:"status"`
		At     string `json:"at"`
	}
	if err := Decode(resp, &w, healthShape); err != nil {
		return nil, err
	}
	if w.Status != "ok" {
		return nil, c.contractFrom(resp, "status", "the liveness status is not ok")
	}
	at, err := time.Parse(time.RFC3339Nano, w.At)
	if err != nil {
		return nil, c.contractFrom(resp, "at", "the liveness timestamp is not an RFC 3339 instant")
	}
	return &Health{Status: w.Status, At: at, RequestID: resp.RequestID}, nil
}

// ---- readiness -------------------------------------------------------------------------------------------------------

// Readiness is a module's readiness answer: GET /readyz.
//
//	module 1 (api.go handleReadyz): {"status":"ready"|"not_ready","checks":{"db","pepper"[,"queue"]},"ready":bool,"config":"...","pepper":"set","supabaseKey":"set","at":...[,"reasons":[...]]}
//	modules 2 and 3 (httpapi/health.go): {"status":"ready"|"not_ready","checks":{"db","singleton_lock"},"db":bool,"singleton_lock":bool,"at":...}
//
// 200 is ready and 503 is not ready, on all three. Readiness is NOT about the work a module does: modules 2 and 3 are ready with no worker, and module 1 is ready with no
// verifier node wired.
type Readiness struct {
	// Ready is true only for HTTP 200 with the module's own status word "ready" and every check true.
	Ready bool
	// Status is the module's word: "ready" or "not_ready".
	Status string
	// HTTPStatus is 200 or 503.
	HTTPStatus int
	// Checks is the module's checks{} map, as it returned it (module 1: db, pepper, queue; modules 2 and 3: db, singleton_lock).
	Checks map[string]bool
	// Reasons is module 1's "reasons" (why a check is false), at most 20 entries of at most 500 bytes; modules 2 and 3 send none.
	Reasons   []string
	RequestID string
}

// Failing lists the names of the checks that are false, sorted.
func (r *Readiness) Failing() []string {
	var out []string
	for name, ok := range r.Checks {
		if !ok {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

var readyShape = Object(
	Req("status", Str),
	Req("checks", MapOf(Bool)),
	Opt("reasons", ArrayOf(Str)),
	Opt("at", Str),
)

const (
	maxReasons     = 20
	maxReasonBytes = 500
)

// judgeReady accepts a 503 that carries a readiness document as an ANSWER (the module said "not ready", and said why); any other 503 (no body, an HTML gateway page, an error
// envelope) goes through the ordinary classification: Unavailable, or Paused for the envelope code `paused`.
func (c *Client) judgeReady(p *prepared, raw *rawResponse) (*Response, *Error) {
	if raw.status == http.StatusServiceUnavailable && !raw.oversize && jsonContentType(raw.header) && conforms(raw.body, readyShape) {
		return c.responseOf(p, raw), nil
	}
	return c.judgeDefault(p, raw)
}

// conforms reports whether body is one JSON document of the shape.
func conforms(body []byte, shape Shape) bool {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var doc any
	if dec.Decode(&doc) != nil {
		return false
	}
	var vs []violation
	shape.check("", doc, &vs)
	return len(vs) == 0
}

// Ready calls GET /readyz (idempotent, no credential sent).
//
// "Not ready" IS AN ANSWER, NOT AN ERROR. A 503 whose body is the module's readiness document comes back as (&Readiness{Ready: false, ...}, nil): the checks map says what is
// failing and the caller decides what to do. A 503 WITHOUT a readiness document (no body, a gateway page, an error envelope) is an error: Unavailable (Paused for the
// envelope code `paused`), and the *Readiness is nil. A 200 that says "not_ready", a 503 that says "ready", a "ready" with a false check, a status word other than the two, or
// a document without `status` and `checks` is a Contract error: the module contradicted itself, and nobody can tell which half to believe.
func (c *Client) Ready(ctx context.Context) (*Readiness, error) {
	resp, e := c.do(ctx, Request{Route: string(c.module) + ".readyz", Method: http.MethodGet, Path: "/readyz", Idempotent: true, Anonymous: true}, c.judgeReady)
	if e != nil {
		return nil, e
	}
	var w struct {
		Status  string          `json:"status"`
		Checks  map[string]bool `json:"checks"`
		Reasons []string        `json:"reasons"`
	}
	if err := Decode(resp, &w, readyShape); err != nil {
		return nil, err
	}
	switch w.Status {
	case "ready":
		if resp.Status != http.StatusOK {
			return nil, c.contractFrom(resp, "status", "the module says ready with a status other than 200")
		}
		names := make([]string, 0, len(w.Checks))
		for name := range w.Checks {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if !w.Checks[name] {
				return nil, c.contractFrom(resp, "checks."+safeKey(name), "the module says ready while a check is false")
			}
		}
	case "not_ready":
		if resp.Status != http.StatusServiceUnavailable {
			return nil, c.contractFrom(resp, "status", "the module says not_ready with a status other than 503")
		}
	default:
		return nil, c.contractFrom(resp, "status", "the readiness status is neither ready nor not_ready")
	}
	r := &Readiness{
		Ready: w.Status == "ready", Status: w.Status, HTTPStatus: resp.Status, RequestID: resp.RequestID,
		Checks: make(map[string]bool, len(w.Checks)),
	}
	for k, v := range w.Checks {
		r.Checks[k] = v
	}
	for i, s := range w.Reasons {
		if i == maxReasons {
			break
		}
		if len(s) > maxReasonBytes {
			s = strings.ToValidUTF8(s[:maxReasonBytes], "")
		}
		r.Reasons = append(r.Reasons, s)
	}
	return r, nil
}

// ---- describe and vocabularies ---------------------------------------------------------------------------------------

// ErrorCode is one entry of a module's published error_codes.
type ErrorCode struct {
	Code       string
	HTTPStatus int
}

// Pricing is the part of a module's published pricing the platform reads.
type Pricing struct {
	Currency         string
	Model            string
	UnitPriceCredits int64
	// Models is the declared value set of Model (additive-only), empty when the module does not publish it.
	Models          []string
	CostFieldsInert bool
}

// Describe is a module's self-description: GET /v1/describe on modules 2 and 3 (their handleDescribe in platformapi/handlers.go), decoded to the keys the two share. Module 3
// publishes more (extension_routes, schemas, bulk_add); those are tolerated and not decoded.
type Describe struct {
	Module     string
	APIVersion string
	// Capabilities are the module's feature flags (single_send, sequences, ...).
	Capabilities map[string]bool
	Pricing      Pricing
	// Vocabularies are the value sets the module's responses can carry (job_state, error_class, item_reason, ...), read live from its database by the module.
	Vocabularies Vocabularies
	ErrorCodes   []ErrorCode
	RequestID    string
}

var describeShape = Object(
	Req("module", Str),
	Req("api_version", Str),
	Req("capabilities", MapOf(Bool)),
	Req("pricing", Object(
		Req("currency", Str),
		Req("model", Str),
		Req("unit_price_credits", Int),
		Opt("models", ArrayOf(Str)),
		Opt("cost_fields_inert", Bool),
	)),
	Req("vocabularies", MapOf(Object(Opt("values", ArrayOf(Str))))),
	Req("error_codes", ArrayOf(Object(Req("code", Str), Req("http_status", Int)))),
)

// notSupported is the error of an operation the module does not have. Nothing is sent.
func (c *Client) notSupported(op, why string) *Error {
	return &Error{Kind: NotSupported, Module: c.module, Route: string(c.module) + "." + op, Message: why}
}

// Describe calls GET /v1/describe (idempotent, with the credential). Module 1 has no describe: a client built for M1 returns an *Error of Kind NotSupported
// (errors.Is(err, ErrNotSupported)) WITHOUT any network call, and the adapter synthesises module 1's vocabulary from constants.
//
// The document is decoded strictly: `module`, `api_version`, `capabilities` (booleans), `pricing` (currency, model, unit_price_credits), `vocabularies` (each an object whose
// `values`, when present, is an array of strings) and `error_codes` ({code, http_status}) must be present with those types, or the call is a Contract error. Keys it does not
// list are ignored. Whether `module` is the module the adapter expects ("send_managed", "send_byo") is the adapter's check: a describe of module 3 answered at module 2's address
// is a wiring mistake that only the adapter knows how to name.
func (c *Client) Describe(ctx context.Context) (*Describe, error) {
	if c.module == M1 {
		return nil, c.notSupported("describe", "module 1 has no describe route (its API has six routes and none describes the module); the adapter supplies its vocabulary from constants")
	}
	resp, e := c.do(ctx, Request{Route: string(c.module) + ".describe", Method: http.MethodGet, Path: "/v1/describe", Idempotent: true}, c.judgeDefault)
	if e != nil {
		return nil, e
	}
	var w struct {
		Module       string          `json:"module"`
		APIVersion   string          `json:"api_version"`
		Capabilities map[string]bool `json:"capabilities"`
		Pricing      struct {
			Currency         string   `json:"currency"`
			Model            string   `json:"model"`
			UnitPriceCredits int64    `json:"unit_price_credits"`
			Models           []string `json:"models"`
			CostFieldsInert  bool     `json:"cost_fields_inert"`
		} `json:"pricing"`
		Vocabularies map[string]struct {
			Values []string `json:"values"`
		} `json:"vocabularies"`
		ErrorCodes []struct {
			Code       string `json:"code"`
			HTTPStatus int    `json:"http_status"`
		} `json:"error_codes"`
	}
	if err := Decode(resp, &w, describeShape); err != nil {
		return nil, err
	}
	d := &Describe{
		Module: w.Module, APIVersion: w.APIVersion, Capabilities: w.Capabilities, RequestID: resp.RequestID,
		Pricing: Pricing{
			Currency: w.Pricing.Currency, Model: w.Pricing.Model, UnitPriceCredits: w.Pricing.UnitPriceCredits,
			Models: w.Pricing.Models, CostFieldsInert: w.Pricing.CostFieldsInert,
		},
		Vocabularies: Vocabularies{},
	}
	for name, v := range w.Vocabularies {
		if v.Values != nil {
			d.Vocabularies[name] = v.Values
		}
	}
	for _, ec := range w.ErrorCodes {
		d.ErrorCodes = append(d.ErrorCodes, ErrorCode{Code: ec.Code, HTTPStatus: ec.HTTPStatus})
	}
	return d, nil
}

// Vocabularies calls Describe and returns the value sets the module advertises (the item states, error classes and reasons its responses can carry), so an adapter can compare
// them with what it was written against (Vocabularies.Diff) and see contract drift before it meets an unknown state. For module 1 it is NotSupported, with no network call.
// ON ANY ERROR THE RESULT IS NIL.
func (c *Client) Vocabularies(ctx context.Context) (Vocabularies, error) {
	d, err := c.Describe(ctx)
	if err != nil {
		return nil, err
	}
	return d.Vocabularies, nil
}

// Vocabularies maps a vocabulary's name (job_state, error_class, item_reason, ...) to the values the module advertises for it. A vocabulary that has no `values` array is absent.
type Vocabularies map[string][]string

// Values returns the advertised values of one vocabulary.
func (v Vocabularies) Values(name string) ([]string, bool) {
	vals, ok := v[name]
	return vals, ok
}

// VocabDiff is the difference between what a module advertises for one vocabulary and what an adapter expects.
type VocabDiff struct {
	Name string
	// Advertised is false when the module publishes no such vocabulary at all.
	Advertised bool
	// Missing are expected values the module does not advertise (a rename or removal: the adapter would mis-handle them; this breaks the contract).
	Missing []string
	// Extra are advertised values the adapter does not expect (an additive change: the adapter must treat the new value as unmapped, never guess).
	Extra []string
}

// Breaking is true when the module no longer offers what the adapter relies on: the vocabulary is gone, or an expected value is missing.
func (d VocabDiff) Breaking() bool { return !d.Advertised || len(d.Missing) > 0 }

// Exact is true when the module advertises exactly the expected values.
func (d VocabDiff) Exact() bool { return d.Advertised && len(d.Missing) == 0 && len(d.Extra) == 0 }

// Diff compares the advertised values of vocabulary name with expected, as sets. Missing and Extra are sorted.
func (v Vocabularies) Diff(name string, expected []string) VocabDiff {
	got, ok := v[name]
	d := VocabDiff{Name: name, Advertised: ok}
	if !ok {
		return d
	}
	have := make(map[string]bool, len(got))
	for _, s := range got {
		have[s] = true
	}
	want := make(map[string]bool, len(expected))
	for _, s := range expected {
		if want[s] {
			continue // a duplicate in the expected list is one value
		}
		want[s] = true
		if !have[s] {
			d.Missing = append(d.Missing, s)
		}
	}
	for s := range have {
		if !want[s] {
			d.Extra = append(d.Extra, s)
		}
	}
	sort.Strings(d.Missing)
	sort.Strings(d.Extra)
	return d
}
