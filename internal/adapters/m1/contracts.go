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
	flagsShape = modclient.Object(modclient.Req("from_cache", modclient.Bool), modclient.Opt("is_disposable", modclient.Bool), modclient.Opt("is_free_provider", modclient.Bool), modclient.Opt("is_role", modclient.Bool), modclient.Opt("is_spam_trap", modclient.Bool)).Open() // flags is a map[string]bool in the module (verdict.go): from_cache always, and the cheap layers' signal keys only when the layer says so (the compare-mode runs on the real node: pages without a disposable or spam-trap row lack those keys)

	// signalsShape: the two keys of the open `signals` bag the charge table reads. Both are absent when they do not apply (failure_origin exists on every `unknown`, smtp_skipped_reason
	// only where the SMTP stage did not run); the bag itself is always an object in a served verdict.
	signalsShape = modclient.Object(
		modclient.Opt("smtp_skipped_reason", modclient.Str),
		modclient.Opt("failure_origin", modclient.Str),
	).Open() // an OPEN bag: the pipeline layers write whichever keys they produced for the row (syntax_valid, mx_found, null_mx, has_typo, engine_mx_hosts, ...), so the keys other than these two differ row by row and are not compared (found by the compare-mode run on the real node)

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

	// submitShape: POST /v1/verify/bulk, 202 (the first answer; a replay of the same key and request is the same body, so it has the same shape: only the response header
	// Idempotent-Replayed differs, and a header is not part of a body contract). counts is over INPUT ROWS; since the Phase 3 close-out the duplicate and malformed counts are also on
	// the job status (jobShape's totals).
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

	// jobShape: GET /v1/jobs/{id}, 200. totals (Phase 3 close-out) is read from the same snapshot as counts and complete: every key of it is required, and the four keys of by_status
	// (the verdict statuses, zero included) too, so a module that stopped sending one cannot make a count read as zero.
	jobShape = modclient.Object(jobFields...)

	// jobFields are the keys of a job-status body, once: GET /v1/jobs/{id} (jobShape) and the answer of POST /v1/jobs/{id}/cancel (cancelShape: the same body plus cancel_outcome) are
	// decoded with the same list. counts.cancelled, totals.cancelled and cancel_requested are ADDITIVE (module 1 migration 0020, owner ruling C2): OPTIONAL here, so that the answer of a
	// module without cancel (and every recorded fixture of it) still decodes, and read as 0 and false when absent; present with another type, or null, they are a Contract error.
	jobFields = []modclient.Field{
		modclient.Req("job_id", modclient.Str),
		modclient.Req("status", modclient.Str),
		modclient.Req("complete", modclient.Bool),
		modclient.Req("counts", modclient.Object(
			modclient.Req("total", modclient.Int),
			modclient.Req("settled", modclient.Int),
			modclient.Req("pending", modclient.Int),
			modclient.Opt("cancelled", modclient.Int),
		)),
		modclient.Req("totals", modclient.Object(
			modclient.Req("submitted", modclient.Int),
			modclient.Req("settled", modclient.Int),
			modclient.Req("pending", modclient.Int),
			modclient.Req("duplicates", modclient.Int),
			modclient.Req("malformed", modclient.Int),
			modclient.Opt("cancelled", modclient.Int),
			modclient.Req("by_status", modclient.Object(
				modclient.Req("valid", modclient.Int),
				modclient.Req("invalid", modclient.Int),
				modclient.Req("risky", modclient.Int),
				modclient.Req("unknown", modclient.Int),
			)),
		)),
		modclient.Req("processing_paused", modclient.Bool),
		modclient.Opt("cancel_requested", modclient.Bool),
	}

	// cancelShape: POST /v1/jobs/{id}/cancel, 200 (ADR-0023): the job-status body (jobFields) plus `cancel_outcome`, required, one of three words (checked by Cancel: a word outside them is
	// a Contract error naming the key). The module's `caller_ref` and every other key it adds are tolerated.
	cancelShape = modclient.Object(append(append([]modclient.Field(nil), jobFields...), modclient.Req("cancel_outcome", modclient.Str))...)

	// describeShape: GET /v1/describe, 200. The family's keys as modclient demands them of a client built for module 1 (module, api_version, capabilities, vocabularies whose entries
	// carry their values, error_codes; no pricing: TestDescribeShapeAgreesWithModclient holds the two together) plus the two keys the adapter reads itself, the route table and the
	// idempotency contract. `schemas` is not listed: the adapter reads none of it, and the drift gate still compares its keys with the fixture's.
	describeShape = modclient.Object(
		modclient.Req("module", modclient.Str),
		modclient.Req("api_version", modclient.Str),
		modclient.Req("capabilities", modclient.MapOf(modclient.Bool)),
		modclient.Req("routes", modclient.ArrayOf(modclient.Object(
			modclient.Req("method", modclient.Str),
			modclient.Req("path", modclient.Str),
			modclient.Opt("id", modclient.Str),
			modclient.Opt("auth", modclient.Str),
			modclient.Opt("description", modclient.Str),
			modclient.Opt("request_schema_ref", modclient.Str),
			modclient.Opt("response_schema_ref", modclient.Str),
		))),
		modclient.Req("vocabularies", modclient.MapOf(modclient.Object(modclient.Req("values", modclient.ArrayOf(modclient.Str))))),
		modclient.Req("error_codes", modclient.ArrayOf(modclient.Object(modclient.Req("code", modclient.Str), modclient.Req("http_status", modclient.Int)))),
		modclient.Req("idempotency", modclient.Object(
			modclient.Req("header", modclient.Str),
			modclient.Req("replay_response_header", modclient.Str),
			modclient.Opt("route", modclient.Str),
			modclient.Opt("key_pattern", modclient.Str),
			modclient.Opt("scope", modclient.Str),
			modclient.Opt("reused_with_other_request", modclient.Str),
		)),
		// vendor_fallback (module 1 migration 0020, owner ruling C3) is ADDITIVE and OPTIONAL here, so that the describe of a module that does not have it (and the recorded fixture of that module) still
		// decodes and reads as "not said" (VendorReachable.Known false); present, it must have the shape of vendorFallbackShape or it is a Contract error naming the key.
		modclient.Opt("vendor_fallback", vendorFallbackShape),
	)

	// rowShape: one element of the results listing: the verdict object plus the row's identity. item_id is required because the adapter always submits the `items` shape, so a result
	// with none cannot be attributed to an input row; duplicate_of is absent on a first row and on every malformed row (and 0 is a real value).
	//
	// flags.from_cache is OPTIONAL in a row (and only in a row: the verify route's flagsShape keeps it required): a row cancelled before its check began (module 1 migration 0020) is
	// served with `"flags": {}`, no verdict and so no cache signal. What it protects is kept by rowOf instead: a row whose status is a VERDICT word and that carries no from_cache is still a
	// Contract error naming results[i].flags.from_cache, so a module that stopped sending the signal cannot make every cached answer read as a fresh one.
	rowShape = modclient.Object(
		modclient.Req("ordinal", modclient.Int),
		modclient.Req("item_id", modclient.Str),
		modclient.Opt("duplicate_of", modclient.Int),
		modclient.Req("status", modclient.Str),
		modclient.Req("reason", modclient.Str),
		modclient.Req("flags", rowFlagsShape),
		modclient.Req("signals", signalsShape),
	)

	// rowFlagsShape is flagsShape with from_cache optional: see rowShape.
	rowFlagsShape = modclient.Object(modclient.Opt("from_cache", modclient.Bool), modclient.Opt("is_disposable", modclient.Bool), modclient.Opt("is_free_provider", modclient.Bool), modclient.Opt("is_role", modclient.Bool), modclient.Opt("is_spam_trap", modclient.Bool)).Open()

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
	tplVerify   = "POST /v1/verify"
	tplSubmit   = "POST /v1/verify/bulk"
	tplStatus   = "GET /v1/jobs/{id}"
	tplResults  = "GET /v1/jobs/{id}/results"
	tplCancel   = "POST /v1/jobs/{id}/cancel"
	tplDescribe = "GET /v1/describe"

	routeVerify   = "m1.verify"
	routeSubmit   = "m1.submit"
	routeStatus   = "m1.status"
	routeResults  = "m1.results"
	routeCancel   = "m1.cancel"
	routeDescribe = "m1.describe" // the label modclient gives its own describe call, so a Contract error of the adapter's checks and one of the decode name the same route
)

// UncoveredRoutes are the routes the adapter calls that Contracts() does not cover, each with why: the drift gate prints them so that "all contracts match" is never read as "every
// route is pinned". A test keeps this list and Contracts() exactly complementary over the routes the adapter calls.
func UncoveredRoutes() map[string]string {
	const why = "passed through modclient (Health, Ready), which decodes it with its own unexported shape; a copy here would be a second definition nothing decodes with"
	out := map[string]string{"GET /healthz": why, "GET /readyz": why}
	if !recordedPauseFixtures {
		// the cancel route was added to the module AFTER the recordings in testdata/contract were made (Phase 4 close-out C2, migration 0020): there is no recorded body of it to pin, and a
		// contract with a hand-written fixture would pin the platform's reading of the documentation, not the module. The lead records it after the pin and moves it into Contracts() by flipping
		// recordedPauseFixtures (below), once the recordings are installed.
		out[tplCancel] = "added to the module after the recordings: no recorded answer yet (its tests use hand-written bodies written from api-contract.md, named HandWritten); promote it to a contract by installing the recordings and flipping recordedPauseFixtures"
	}
	return out
}

// recordedPauseFixtures is THE SWITCH of the Phase 4 D1 recordings of module 1 (its name is the one the module 2 and module 3 switch has, sendapi's recordedPauseFixtures; this one is about module 1's
// CANCEL). The answers of POST /v1/jobs/{id}/cancel and the results page that holds a cancelled row are contracts (CancelContracts), and the contract drift session of the harness records them in
// record mode and compares them in compare mode; but until the lead has installed the three recordings (CancelFixtureNames) a contract without a fixture would be red in the no-rot check, and a
// fixture written by hand would be a recording nobody made. So while this is false the contracts are DECLARED but not LISTED: Contracts leaves them out and UncoveredRoutes names the cancel route. The lead
// flips it, as the ONLY edit of the switch, after installing the files: TestCancelFixturesAreInstalledWhenTheSwitchIsOn is red when it is true and a file is missing, and
// TestCancelFixturesDoNotExistWhileTheSwitchIsOff is red when it is false and one of them is there, so neither half of the change can be made without the other.
const recordedPauseFixtures = true

// RecordedPauseFixtures says whether module 1's cancel recordings are installed and their contracts listed (the value of the switch).
func RecordedPauseFixtures() bool { return recordedPauseFixtures }

// CancelFixtureNames are the three files the switch is about (under testdata/contract): the 200 of a cancel (ONE shape for the three outcomes `cancelled`, `already_cancelled` and
// `already_complete`: the job-status body plus the outcome word, so one contract and one file; which outcome the recorder kept is in the file), the 404 of a cancel of a job that does not exist, and
// a results page that holds a row cancelled before its check began (status `cancelled`, reason `cancelled_by_caller`, an empty `flags`).
func CancelFixtureNames() []string {
	return []string{"cancel-ok.json", "cancel-error-not-found.json", "results-page-cancelled.json"}
}

// CancelContracts are the contracts of module 1's cancel recordings, DECLARED whether or not they are listed (Contracts lists them when recordedPauseFixtures is true; the harness resolves the contracts
// of its steps here so that record mode can print a recording under the right name before the files exist):
//
//   - POST /v1/jobs/{id}/cancel `ok`: the 200, decoded with cancelShape; its `status` is a word of the job_status vocabulary like any job's. One contract for the three outcomes (they differ in no key
//     or type, and a vocabulary of the outcome words would have to be published under a name of its own).
//   - POST /v1/jobs/{id}/cancel `not_found`: the 404 envelope of a job that does not exist (or is another caller's).
//   - GET /v1/jobs/{id}/results `page_cancelled`: a page that holds a cancelled row. It lists NO status or reason vocabulary: the page mixes verdict rows (words of the adapter's sets) with cancelled
//     rows (the non-verdict words `cancelled` and `cancelled_by_caller`, which are no member of those sets), and the sets Contracts publishes are exactly the adapter's switches; the two
//     non-verdict words are held by the adapter's own decoding, by TestNonVerdictVocabListsAreTheSwitches and by the comparison with the module's describe.
//
// The status of a job after a cancel is the same body as before (counts.cancelled, totals.cancelled and cancel_requested are keys of the recorded `running` and `complete` fixtures), so it needs no
// contract of its own.
func CancelContracts() []modclient.RouteContract {
	m1 := modclient.M1
	ok := modclient.RouteContract{Module: m1, Route: tplCancel, Case: "ok", Fixture: "cancel-ok.json", Status: http.StatusOK, Shape: cancelShape,
		Vocab: map[string][]string{"job_status": vocabSets()["job_status"]}, VocabAt: map[string]string{"job_status": "status"}}
	notFound := modclient.RouteContract{Module: m1, Route: tplCancel, Case: "not_found", Fixture: "cancel-error-not-found.json", Status: http.StatusNotFound, Shape: errorShape,
		Vocab: map[string][]string{"error_code": append([]string(nil), errorCodeValues...)}, VocabAt: map[string]string{"error_code": "error.code"}}
	page := modclient.RouteContract{Module: m1, Route: tplResults, Case: "page_cancelled", Fixture: "results-page-cancelled.json", Status: http.StatusOK, Shape: resultsShape}
	return []modclient.RouteContract{ok, notFound, page}
}

// PendingContracts are the contracts that are declared (CancelContracts) and not listed yet: all of them while recordedPauseFixtures is false, none once it is true. The harness's drift session
// compares these with a fixture only when its file exists, and prints them in record mode.
func PendingContracts() []modclient.RouteContract {
	if recordedPauseFixtures {
		return nil
	}
	return CancelContracts()
}

// errorCodeValues is the code of module 1's error envelope (api/errors.go: the ten `code*` constants, additive-only; `idempotency_key_reused` came with the idempotent create). The
// adapter has ONE switch on it: a 409 whose code is idempotency_key_reused is ErrIdempotencyKeyReused (Submit); for every other code modclient decides the class of an error by
// its status and reads the code as data (only `paused` changes a class), so a code module 1 adds later is carried and never refused. The list is published for the drift gate on
// every error contract at `error.code` and on the describe contract at `error_codes[].code`, so that a new code in a live answer is reported, and CheckVocabularies reports a code
// of it that the module no longer lists.
var errorCodeValues = []string{
	"unauthenticated", "validation_failed", "not_found", "method_not_allowed", "payload_too_large", "unavailable", "paused", "internal_error", "contract_violation",
	"idempotency_key_reused",
}

// Contracts lists every module 1 response the adapter relies on: one entry per route and per case whose shape or vocabulary differs (the success of each route, a page with rows and
// one without, a running and a finished job, and the error envelope of every class the adapter reacts to). Fixture names a file under testdata/contract/; Shape is the value the
// adapter decodes that response with; Vocab holds the closed sets the adapter maps from, as the switches of vocab.go handle them, and VocabAt the JSON path of the string position
// each set constrains IN THAT RESPONSE (a bare set name is read by the drift gate as a path to a top-level key, which most of these bodies do not have).
//
// A set is listed on a contract only when the contract's recorded body has a string at that path: a vocabulary that constrains nothing in its own fixture would be inert. The
// signals of an unknown or degraded answer (failure_origin, smtp_skipped_reason) are in no recorded verdict (the one synchronous verdict is a valid one); the 7-row results page recorded
// in the Phase 3 close-out D1 DOES hold two `failure_origin: remote` rows and one skip token, but they sit in the open `signals` bag, which the Shape does not list key by key, so they
// are listed only on the describe contract, where module 1 publishes every set it has (`vocabularies.<name>.values[]`): that is the one body
// that holds all seven the adapter maps, and the live drift gate compares the live values of each with the adapter's. The results page recorded WITHOUT rows (case empty_page) lists
// no row set at all (it has no row to hold a status). The describe contract also lists the error codes at `error_codes[].code`. The replay of an idempotent create is the same body
// as its first answer and is not a contract of its own.
//
// Module 1's two probe routes, /healthz and /readyz, are NOT listed: the adapter passes them through modclient (Health, Ready), which decodes them with shapes it does not export,
// so listing a copy here would be a second definition that nothing decodes with (UncoveredRoutes says so).
func Contracts() []modclient.RouteContract {
	// with fills Vocab and VocabAt of rc from pairs of (set name, path): the values come from vocabSets (or errorCodeValues), the path is where the set lives in rc's body.
	with := func(rc modclient.RouteContract, pairs ...string) modclient.RouteContract {
		all := vocabSets()
		all["error_code"] = append([]string(nil), errorCodeValues...)
		rc.Vocab, rc.VocabAt = map[string][]string{}, map[string]string{}
		for i := 0; i < len(pairs); i += 2 {
			rc.Vocab[pairs[i]] = all[pairs[i]]
			rc.VocabAt[pairs[i]] = pairs[i+1]
		}
		return rc
	}
	errAt := func(rc modclient.RouteContract) modclient.RouteContract { return with(rc, "error_code", "error.code") }
	described := func(name string) string { return "vocabularies." + name + ".values[]" } // where the describe body holds the values of one vocabulary
	m1 := modclient.M1
	cs := []modclient.RouteContract{
		with(modclient.RouteContract{Module: m1, Route: tplVerify, Case: "ok", Fixture: "verify-ok.json", Status: http.StatusOK, Shape: verifyShape},
			"status", "status", "reason", "reason", "recommendation", "recommendation"),

		with(modclient.RouteContract{Module: m1, Route: tplSubmit, Case: "accepted", Fixture: "submit-accepted.json", Status: http.StatusAccepted, Shape: submitShape}, "submit_status", "status"),
		errAt(modclient.RouteContract{Module: m1, Route: tplSubmit, Case: "validation_failed", Fixture: "submit-error-validation.json", Status: http.StatusBadRequest, Shape: errorShape}),
		errAt(modclient.RouteContract{Module: m1, Route: tplSubmit, Case: "unauthenticated", Fixture: "submit-error-unauthenticated.json", Status: http.StatusUnauthorized, Shape: errorShape}),
		errAt(modclient.RouteContract{Module: m1, Route: tplSubmit, Case: "idempotency_key_reused", Fixture: "submit-error-key-reused.json", Status: http.StatusConflict, Shape: errorShape}),
		errAt(modclient.RouteContract{Module: m1, Route: tplSubmit, Case: "payload_too_large", Fixture: "submit-error-too-large.json", Status: http.StatusRequestEntityTooLarge, Shape: errorShape}),
		errAt(modclient.RouteContract{Module: m1, Route: tplSubmit, Case: "paused", Fixture: "submit-error-paused.json", Status: http.StatusServiceUnavailable, Shape: errorShape}),
		errAt(modclient.RouteContract{Module: m1, Route: tplSubmit, Case: "unavailable", Fixture: "submit-error-unavailable.json", Status: http.StatusServiceUnavailable, Shape: errorShape}),
		errAt(modclient.RouteContract{Module: m1, Route: tplSubmit, Case: "internal_error", Fixture: "submit-error-internal.json", Status: http.StatusInternalServerError, Shape: errorShape}),

		with(modclient.RouteContract{Module: m1, Route: tplStatus, Case: "running", Fixture: "job-running.json", Status: http.StatusOK, Shape: jobShape}, "job_status", "status"),
		with(modclient.RouteContract{Module: m1, Route: tplStatus, Case: "complete", Fixture: "job-complete.json", Status: http.StatusOK, Shape: jobShape}, "job_status", "status"),
		errAt(modclient.RouteContract{Module: m1, Route: tplStatus, Case: "not_found", Fixture: "status-error-not-found.json", Status: http.StatusNotFound, Shape: errorShape}),
		errAt(modclient.RouteContract{Module: m1, Route: tplStatus, Case: "unauthenticated", Fixture: "status-error-unauthenticated.json", Status: http.StatusUnauthorized, Shape: errorShape}),

		with(modclient.RouteContract{Module: m1, Route: tplResults, Case: "page", Fixture: "results-page.json", Status: http.StatusOK, Shape: resultsShape},
			"status", "results[].status", "reason", "results[].reason"),
		// the page the real node answered under this case holds NO row: no row set can be listed
		{Module: m1, Route: tplResults, Case: "empty_page", Fixture: "results-empty.json", Status: http.StatusOK, Shape: resultsShape},
		errAt(modclient.RouteContract{Module: m1, Route: tplResults, Case: "bad_cursor", Fixture: "results-error-bad-cursor.json", Status: http.StatusBadRequest, Shape: errorShape}),
		errAt(modclient.RouteContract{Module: m1, Route: tplResults, Case: "unauthenticated", Fixture: "results-error-unauthenticated.json", Status: http.StatusUnauthorized, Shape: errorShape}),
		errAt(modclient.RouteContract{Module: m1, Route: tplResults, Case: "not_found", Fixture: "results-error-not-found.json", Status: http.StatusNotFound, Shape: errorShape}),

		with(modclient.RouteContract{Module: m1, Route: tplDescribe, Case: "ok", Fixture: "describe-ok.json", Status: http.StatusOK, Shape: describeShape},
			"status", described("status"), "reason", described("reason"), "recommendation", described("recommendation"), "failure_origin", described("failure_origin"),
			"smtp_skipped_reason", described("smtp_skipped_reason"), "job_status", described("job_status"), "submit_status", described("submit_status"),
			"error_code", "error_codes[].code"),
		errAt(modclient.RouteContract{Module: m1, Route: tplDescribe, Case: "unauthenticated", Fixture: "describe-error-unauthenticated.json", Status: http.StatusUnauthorized, Shape: errorShape}),
	}
	if recordedPauseFixtures { // module 1's cancel recordings (CancelContracts): listed once they are installed
		cs = append(cs, CancelContracts()...)
	}
	return cs
}
