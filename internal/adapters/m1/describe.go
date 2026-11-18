package m1

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/coldmail-platform/platform/internal/modclient"
)

// describe.go: GET /v1/describe (module 1 since the Phase 3 close-out, partial Tier 4): the module says what it may say, in the shape family of modules 2 and 3, and the adapter asks
// it exactly as internal/adapters/sendapi asks them (Describe, then CheckVocabularies). The adapter still HOLDS the seven vocabularies it maps from as constants (vocab.go: its
// decoding is strict, so a value it does not know is refused at decode time); the describe is how a change of the module is seen before a live row meets it.

const (
	// moduleName is what module 1's describe says as `module` (the senders say send_managed and send_byo).
	moduleName = "verify"
	// apiVersion is the version the adapter was written against.
	apiVersion = "v1"
	// idempotencyHeader is the request header of the idempotent bulk create; replayedHeader is the response header that marks a replay. Both are also published by describe
	// (`idempotency.header`, `idempotency.replay_response_header`), and Describe checks that they are still these words.
	idempotencyHeader = "Idempotency-Key"
	replayedHeader    = "Idempotent-Replayed"

	// vocabErrorCodes names the error codes in a report's Key.
	vocabErrorCodes = "error_codes"
)

// calledRoutes are the routes the adapter calls, as describe lists them (method and path template): Describe refuses a module that does not serve one of them.
var calledRoutes = []struct{ method, path string }{
	{http.MethodPost, "/v1/verify"},
	{http.MethodPost, "/v1/verify/bulk"},
	{http.MethodGet, "/v1/jobs/{id}"},
	{http.MethodGet, "/v1/jobs/{id}/results"},
}

// describeExtra is the part of the describe the adapter reads beyond what modclient decodes: the idempotency contract and the route table (modclient.Describe.Raw holds the body).
type describeExtra struct {
	Routes []struct {
		Method string `json:"method"`
		Path   string `json:"path"`
	} `json:"routes"`
	Idempotency struct {
		Header               string `json:"header"`
		ReplayResponseHeader string `json:"replay_response_header"`
	} `json:"idempotency"`
}

// Describe reads module 1's self-description (GET /v1/describe: one idempotent read, retried per the dials) and checks that it is the module the adapter was written for and offers
// what the adapter now relies on, each failure a Contract error that names the key: `module` is "verify" (a describe of another module at this address is a wiring mistake), `api_version`
// is "v1", `capabilities.idempotent_bulk_create` and `capabilities.job_totals` are true (a module without them would make Submit's retries and Status's totals meaningless),
// `idempotency.header` is "Idempotency-Key" and `idempotency.replay_response_header` is "Idempotent-Replayed" (the two names Submit sends and reads), and `routes` lists every route the
// adapter calls. ON ANY ERROR THE RESULT IS NIL.
//
// The describe needs the per-caller key (module 1 answers the legacy static token with a 401, which arrives as a Refused error): an Adapter always carries one.
func (a *Adapter) Describe(ctx context.Context) (*modclient.Describe, error) {
	d, err := a.c.Describe(ctx)
	if err != nil {
		return nil, err
	}
	contract := func(key, msg string) (*modclient.Describe, error) {
		e := newErr(modclient.Contract, routeDescribe, msg)
		e.Key, e.Sent, e.Status, e.RequestID = key, true, http.StatusOK, d.RequestID
		return nil, e
	}
	switch {
	case d.Module != moduleName:
		return contract("module", "the describe names another module than module 1 (the base URL or the key of the wrong module)")
	case d.APIVersion != apiVersion:
		return contract("api_version", "the module serves another API version than the one the adapter was written against")
	case !d.Capabilities["idempotent_bulk_create"]:
		return contract("capabilities.idempotent_bulk_create", "the module does not say its bulk create is idempotent, so a submission could not be retried safely")
	case !d.Capabilities["job_totals"]:
		return contract("capabilities.job_totals", "the module does not say its job status carries totals")
	}
	var x describeExtra
	if err := modclient.Decode(&modclient.Response{Status: http.StatusOK, Body: d.Raw}, &x, describeShape); err != nil {
		if e := asModclient(err); e != nil {
			e.Module, e.Route, e.RequestID, e.Sent = modclient.M1, routeDescribe, d.RequestID, true
		}
		return nil, err
	}
	switch {
	case x.Idempotency.Header != idempotencyHeader:
		return contract("idempotency.header", "the module names another request header for its idempotency key than the one Submit sends")
	case x.Idempotency.ReplayResponseHeader != replayedHeader:
		return contract("idempotency.replay_response_header", "the module names another response header for a replay than the one Submit reads")
	}
	for _, want := range calledRoutes {
		found := false
		for _, r := range x.Routes {
			found = found || (r.Method == want.method && r.Path == want.path)
		}
		if !found {
			return contract("routes", "the module does not list a route the adapter calls: "+want.method+" "+want.path)
		}
	}
	return d, nil
}

// VocabReport is what a describe says against what the adapter handles: the module 1 counterpart of sendapi.VocabReport.
type VocabReport struct {
	// Diffs has one entry per vocabulary the adapter handles (status, reason, recommendation, failure_origin, smtp_skipped_reason, job_status, submit_status), in that order.
	Diffs []modclient.VocabDiff
	// CodesMissing are the error codes the adapter knows (errorCodeValues) that the module no longer lists; CodesExtra are codes it lists that the adapter does not know (additive:
	// modclient decides the class of an error by its status, so a new code is carried as data).
	CodesMissing []string
	CodesExtra   []string
	// VocabulariesExtra are the vocabularies the module advertises that the adapter does not handle (detail_issue today): additive.
	VocabulariesExtra []string
}

// Breaking is true when the module no longer offers what the adapter relies on or offers something it would refuse: a vocabulary that is gone, a value of one missing, an EXTRA value
// in one (EVERY vocabulary of module 1 is strict: the adapter's known* functions refuse an unknown value at decode time, so a value the module may now say is a Contract error on a
// live row, found here before it), or an error code the adapter maps that is gone.
func (r VocabReport) Breaking() bool { return r.firstBreak() != "" }

// Additive is true when the module advertises something the adapter carries without handling: an extra error code, or a vocabulary the adapter does not map. Nothing breaks, and
// a person should look.
func (r VocabReport) Additive() bool { return len(r.CodesExtra) > 0 || len(r.VocabulariesExtra) > 0 }

// firstBreak names the first break ("" when none): "vocabularies.<name>" or "error_codes".
func (r VocabReport) firstBreak() string {
	for _, d := range r.Diffs {
		if d.Breaking() || len(d.Extra) > 0 {
			return "vocabularies." + d.Name
		}
	}
	if len(r.CodesMissing) > 0 {
		return vocabErrorCodes
	}
	return ""
}

// CompareVocabularies is the pure comparison: the describe a module answered against the sets the adapter handles (vocabSets) and the error codes it knows (errorCodeValues).
func CompareVocabularies(d *modclient.Describe) VocabReport {
	var r VocabReport
	sets := vocabSets()
	for _, name := range vocabOrder {
		r.Diffs = append(r.Diffs, d.Vocabularies.Diff(name, sets[name]))
	}
	for name := range d.Vocabularies {
		if _, handled := sets[name]; !handled {
			r.VocabulariesExtra = append(r.VocabulariesExtra, name)
		}
	}
	have := map[string]bool{}
	for _, ec := range d.ErrorCodes {
		have[ec.Code] = true
	}
	for _, code := range errorCodeValues {
		if !have[code] {
			r.CodesMissing = append(r.CodesMissing, code)
		}
	}
	for code := range have {
		if !slices.Contains(errorCodeValues, code) {
			r.CodesExtra = append(r.CodesExtra, code)
		}
	}
	sort.Strings(r.VocabulariesExtra)
	sort.Strings(r.CodesMissing)
	sort.Strings(r.CodesExtra)
	return r
}

// CheckVocabularies reads module 1's describe (Describe, with its own checks) and compares it with what the adapter handles. A break (see VocabReport.Breaking) is a Contract error
// naming the vocabulary in Key and, in the message, the values the ADAPTER expects and the module no longer lists (never a value the module sent); the report is then not returned.
// An additive difference is not an error: it is in the returned report. ON ANY ERROR THE REPORT IS THE ZERO VALUE.
func (a *Adapter) CheckVocabularies(ctx context.Context) (VocabReport, error) {
	d, err := a.Describe(ctx)
	if err != nil {
		return VocabReport{}, err
	}
	r := CompareVocabularies(d)
	key := r.firstBreak()
	if key == "" {
		return r, nil
	}
	msg := "the module's describe no longer matches what the adapter handles"
	for _, df := range r.Diffs {
		if "vocabularies."+df.Name != key {
			continue
		}
		switch {
		case !df.Advertised:
			msg = "the module no longer advertises this vocabulary"
		case len(df.Missing) > 0:
			msg = fmt.Sprintf("the module no longer lists the value(s) the adapter handles: %s", strings.Join(df.Missing, ", "))
		default:
			msg = fmt.Sprintf("the module lists %d value(s) of this closed set that the adapter does not handle (a value it does not know is refused, never guessed)", len(df.Extra))
		}
	}
	if key == vocabErrorCodes {
		msg = fmt.Sprintf("the module no longer lists the error code(s) the adapter maps: %s", strings.Join(r.CodesMissing, ", "))
	}
	e := newErr(modclient.Contract, routeDescribe, msg)
	e.Key, e.Sent, e.Status, e.RequestID = key, true, http.StatusOK, d.RequestID
	return VocabReport{}, e
}
