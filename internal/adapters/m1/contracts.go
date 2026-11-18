package m1

import (
	"net/http"

	"github.com/coldmail-platform/platform/internal/modclient"
)

// The shapes the adapter decodes module 1's answers with. Each is defined ONCE, here, and is the value both the decoding code and Contracts() use. A key the shape does not list is
// tolerated (module 1's API changes are additive); a key it lists as required is the key the adapter reads, so a missing or retyped one is a Contract error naming it, never a zero
// field that reads as an answer.
var (
	// flagsShape: flags.from_cache is the cache signal the charge table reads (and the only reliable one: meta.from_cache is always false in a bulk result, M1-F6). It is required so
	// that a module that stopped sending it cannot make every cached answer read as a fresh one (a different unit price).
	flagsShape = modclient.Object(modclient.Req("from_cache", modclient.Bool), modclient.Opt("is_disposable", modclient.Bool), modclient.Opt("is_free_provider", modclient.Bool), modclient.Opt("is_role", modclient.Bool), modclient.Opt("is_spam_trap", modclient.Bool)) // flags is a map[string]bool in the module (verdict.go): from_cache always, and the cheap layers' signal keys only when the layer says so (the compare-mode runs on the real node: pages without a disposable or spam-trap row lack those keys)

	// signalsShape: the two keys of the open `signals` bag the charge table reads. Both are absent when they do not apply (failure_origin exists on every `unknown`, smtp_skipped_reason
	// only where the SMTP stage did not run); the bag itself is always an object in a served verdict.
	signalsShape = modclient.Object(
		modclient.Opt("smtp_skipped_reason", modclient.Str),
		modclient.Opt("failure_origin", modclient.Str),
	)

	// verifyShape: POST /v1/verify, 200. The keys of the verdict object the adapter returns to the caller (api.go handleVerify, verdict.Response).
	verifyShape = modclient.Object(
		modclient.Req("status", modclient.Str),
		modclient.Req("reason", modclient.Str),
		modclient.Req("confidence", modclient.Int),
		modclient.Req("recommendation", modclient.Str),
		modclient.Req("domain", modclient.Str),
		modclient.Req("flags", flagsShape),
		modclient.Req("signals", signalsShape),
	)

	// submitShape: POST /v1/verify/bulk, 202. counts is over INPUT ROWS and is the only place the duplicate and malformed counts are ever reported (M1-F8).
	submitShape = modclient.Object(
		modclient.Req("job_id", modclient.Str),
		modclient.Req("status", modclient.Str),
		modclient.Req("counts", modclient.Object(
			modclient.Req("total", modclient.Int),
			modclient.Req("enqueued", modclient.Int),
			modclient.Req("duplicate", modclient.Int),
			modclient.Req("malformed", modclient.Int),
		)),
	)

	// jobShape: GET /v1/jobs/{id}, 200.
	jobShape = modclient.Object(
		modclient.Req("job_id", modclient.Str),
		modclient.Req("status", modclient.Str),
		modclient.Req("complete", modclient.Bool),
		modclient.Req("counts", modclient.Object(
			modclient.Req("total", modclient.Int),
			modclient.Req("settled", modclient.Int),
			modclient.Req("pending", modclient.Int),
		)),
		modclient.Req("processing_paused", modclient.Bool),
	)

	// rowShape: one element of the results listing: the verdict object plus the row's identity. item_id is required because the adapter always submits the `items` shape, so a result
	// with none cannot be attributed to an input row; duplicate_of is absent on a first row and on every malformed row (and 0 is a real value).
	rowShape = modclient.Object(
		modclient.Req("ordinal", modclient.Int),
		modclient.Req("item_id", modclient.Str),
		modclient.Opt("duplicate_of", modclient.Int),
		modclient.Req("status", modclient.Str),
		modclient.Req("reason", modclient.Str),
		modclient.Req("flags", flagsShape),
		modclient.Req("signals", signalsShape),
	)

	// resultsShape: GET /v1/jobs/{id}/results, 200. results is always an array (empty is [], never null) and next_cursor always a string ("" when nothing follows as the listing stands).
	resultsShape = modclient.Object(
		modclient.Req("job_id", modclient.Str),
		modclient.Req("results", modclient.ArrayOf(rowShape)),
		modclient.Req("next_cursor", modclient.Str),
	)

	// errorShape: module 1's error envelope (api/errors.go). The adapter never decodes it itself (modclient reads it into a typed error); it is listed so the drift gate compares the
	// envelope of every error class the adapter reacts to.
	errorShape = modclient.Object(
		modclient.Req("error", modclient.Object(
			modclient.Req("code", modclient.Str),
			modclient.Req("message", modclient.Str),
			modclient.Opt("details", modclient.ArrayOf(modclient.Object(modclient.Req("field", modclient.Str), modclient.Req("issue", modclient.Str)))),
			modclient.Req("request_id", modclient.Str),
		)),
	)
)

// The routes as the contracts name them (method and path template, never a URL) and as errors name them (a modclient route label).
const (
	tplVerify  = "POST /v1/verify"
	tplSubmit  = "POST /v1/verify/bulk"
	tplStatus  = "GET /v1/jobs/{id}"
	tplResults = "GET /v1/jobs/{id}/results"

	routeVerify  = "m1.verify"
	routeSubmit  = "m1.submit"
	routeStatus  = "m1.status"
	routeResults = "m1.results"
)

// UncoveredRoutes are the routes the adapter calls that Contracts() does not cover, each with why: the drift gate prints them so that "all contracts match" is never read as "every
// route is pinned". A test keeps this list and Contracts() exactly complementary over the routes the adapter calls.
func UncoveredRoutes() map[string]string {
	const why = "passed through modclient (Health, Ready), which decodes it with its own unexported shape; a copy here would be a second definition nothing decodes with"
	return map[string]string{"GET /healthz": why, "GET /readyz": why}
}

// Contracts lists every module 1 response the adapter relies on: one entry per route and per case whose shape or vocabulary differs (the success of each route, an empty page, a
// running and a finished job, and the error envelope of every class the adapter reacts to). Fixture names a file under testdata/contract/; Shape is the value the adapter decodes
// that response with; Vocab holds the closed sets the adapter maps from, as the switches of vocab.go handle them. Module 1's two probe routes, /healthz and /readyz, are NOT listed:
// the adapter passes them through modclient (Health, Ready), which decodes them with shapes it does not export, so listing a copy here would be a second definition that nothing
// decodes with (see the report; UncoveredRoutes says so).
func Contracts() []modclient.RouteContract {
	set := func(names ...string) map[string][]string {
		all := vocabSets()
		out := make(map[string][]string, len(names))
		for _, n := range names {
			out[n] = all[n]
		}
		return out
	}
	rowVocab := func() map[string][]string { return set("status", "reason", "failure_origin", "smtp_skipped_reason") }
	m1 := modclient.M1
	return []modclient.RouteContract{
		{Module: m1, Route: tplVerify, Case: "ok", Fixture: "verify-ok.json", Status: http.StatusOK, Shape: verifyShape,
			Vocab: set("status", "reason", "failure_origin", "smtp_skipped_reason", "recommendation")},

		{Module: m1, Route: tplSubmit, Case: "accepted", Fixture: "submit-accepted.json", Status: http.StatusAccepted, Shape: submitShape, Vocab: set("submit_status")},
		{Module: m1, Route: tplSubmit, Case: "validation_failed", Fixture: "submit-error-validation.json", Status: http.StatusBadRequest, Shape: errorShape},
		{Module: m1, Route: tplSubmit, Case: "unauthenticated", Fixture: "submit-error-unauthenticated.json", Status: http.StatusUnauthorized, Shape: errorShape},
		{Module: m1, Route: tplSubmit, Case: "payload_too_large", Fixture: "submit-error-too-large.json", Status: http.StatusRequestEntityTooLarge, Shape: errorShape},
		{Module: m1, Route: tplSubmit, Case: "paused", Fixture: "submit-error-paused.json", Status: http.StatusServiceUnavailable, Shape: errorShape},
		{Module: m1, Route: tplSubmit, Case: "unavailable", Fixture: "submit-error-unavailable.json", Status: http.StatusServiceUnavailable, Shape: errorShape},
		{Module: m1, Route: tplSubmit, Case: "internal_error", Fixture: "submit-error-internal.json", Status: http.StatusInternalServerError, Shape: errorShape},

		{Module: m1, Route: tplStatus, Case: "running", Fixture: "job-running.json", Status: http.StatusOK, Shape: jobShape, Vocab: set("job_status")},
		{Module: m1, Route: tplStatus, Case: "complete", Fixture: "job-complete.json", Status: http.StatusOK, Shape: jobShape, Vocab: set("job_status")},
		{Module: m1, Route: tplStatus, Case: "not_found", Fixture: "status-error-not-found.json", Status: http.StatusNotFound, Shape: errorShape},
		{Module: m1, Route: tplStatus, Case: "unauthenticated", Fixture: "status-error-unauthenticated.json", Status: http.StatusUnauthorized, Shape: errorShape},

		{Module: m1, Route: tplResults, Case: "page", Fixture: "results-page.json", Status: http.StatusOK, Shape: resultsShape, Vocab: rowVocab()},
		{Module: m1, Route: tplResults, Case: "empty_page", Fixture: "results-empty.json", Status: http.StatusOK, Shape: resultsShape, Vocab: rowVocab()},
		{Module: m1, Route: tplResults, Case: "bad_cursor", Fixture: "results-error-bad-cursor.json", Status: http.StatusBadRequest, Shape: errorShape},
		{Module: m1, Route: tplResults, Case: "unauthenticated", Fixture: "results-error-unauthenticated.json", Status: http.StatusUnauthorized, Shape: errorShape},
		{Module: m1, Route: tplResults, Case: "not_found", Fixture: "results-error-not-found.json", Status: http.StatusNotFound, Shape: errorShape},
	}
}
