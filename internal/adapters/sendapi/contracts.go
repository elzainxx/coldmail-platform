package sendapi

import (
	"slices"

	"github.com/coldmail-platform/platform/internal/modclient"
)

// The routes as the contracts name them: the method and the path template (never a real URL).
const (
	RouteQuote    = "POST /v1/quote"
	RouteRun      = "POST /v1/run"
	RouteReport   = "GET /v1/report"
	RouteDescribe = "GET /v1/describe"
	// RouteCancel is the route of a module with a cancel (Profile.Cancel): module 2, and module 3 since Phase 3 close-out C1.
	RouteCancel = "POST /v1/cancel"
)

// Where each vocabulary lives in a body, in the path grammar of internal/contractcheck (a.b is a key, x[] is every element of an array): the string-valued position the closed set
// constrains. The live drift gate reads the real response's strings at that path against the set, so a name with no path here would constrain nothing.
const (
	// PathState: the `state` of a run answer and of a single-job report.
	PathState = "state"
	// PathItemState, PathItemErrorClass and PathItemReason: the same keys of every item of a campaign page.
	PathItemState      = "items.results[].state"
	PathItemErrorClass = "items.results[].error_class"
	PathItemReason     = "items.results[].reason"
	// PathErrorCode: the `code` of the module's error envelope, in every error body.
	PathErrorCode = "error.code"
	// PathDescribeErrorCode: the `code` of every entry of describe's error_codes.
	PathDescribeErrorCode = "error_codes[].code"
	// PathConflictCause: the `code` of every entry of the details of a run 409 that names its causes (module 3): one of run_conflict_cause.
	PathConflictCause = "error.details[].code"
)

// place puts one vocabulary (by its describe name) at a path of a body.
type place struct{ name, path string }

// Contracts lists every module response the shared body relies on, for the profile's module: one entry per route and per response whose shape differs (success, the replay of run, an
// empty page, each error the adapter maps). The Shape of each entry is the Shape the client decodes that response with: it is built by the same newShapes that New uses (a test holds the two
// equal), so the offline tests, the live drift gate and the production decoder cannot disagree. Fixture names a file under the adapter package's testdata/contract/ directory: the
// adapter package (m2, m3) holds the recorded bodies; the names are the same for both modules.
//
// An error entry's Shape is the error envelope the platform relies on; the envelope itself is parsed by modclient (a 4xx whose body claims to be an envelope and is not is a Contract error),
// so what the adapter adds to an error is its Kind and Code, which the tests assert per entry.
//
// VOCABULARIES. A contract lists a vocabulary (Vocab, the closed set; VocabAt, the path it constrains) only where its body has a string-valued position for it that EVERY recording of
// that case holds a string at, whichever module's: the single-job report lists only job_state (the module sends error_class and reason as null or omits them for a job that did not fail,
// and module 3 omits them for a job that did, so a recording of either has no string there); the page of a campaign lists job_state, error_class and, for a module with reasons, item_reason
// (a page that holds a failed or bounced item); the last page lists only job_state; the empty page lists nothing (it has no item); the quote lists nothing (it has no state, class or
// code); describe lists only error_codes, because its other vocabularies sit under keys of a map that the drift gate's grammar folds (`vocabularies{}`), so no path reaches one of them
// alone: those are compared with CheckVocabularies, against the live describe, at run time. Every error body lists error_codes at error.code. A vocabulary the profile has no set for
// (item_reason on module 3) is not listed. The 409 on run of a module that names its causes lists run_conflict_cause at error.details[].code. A profile with a cancel route (Profile.Cancel) also
// lists its two cancel responses (the position, and the 400 of a missing or malformed id).
//
// NOT LISTED YET (Phase 4 D1): the answers of POST /v1/pause and POST /v1/resume, and the 409 `job_item_cancelled` of run, have no recorded fixture (the recordings of the two modules at their new
// heads are made after the pin, and the names are the same for both modules), so no contract names them: a contract without a fixture would be red in every adapter's no-rot check, and a
// fixture written by hand would be a recording nobody made. Their shapes exist (shapeSet.pause) and are tested with hand-written bodies (the HandWritten tests); the report's additive
// `paused` is already in the page Shape (optional: every recorded page of an older module still decodes).
//
// Contracts returns nil for a profile that fails validation: a subject that is empty is red in every test that counts it.
func Contracts(p Profile) []modclient.RouteContract {
	if p.validate() != nil {
		return nil
	}
	sh := newShapes(p)
	all := p.Vocabularies()
	mod := p.Module
	rc := func(route, name, fixture string, status int, shape modclient.Shape, at ...place) modclient.RouteContract {
		c := modclient.RouteContract{Module: mod, Route: route, Case: name, Fixture: fixture, Status: status, Shape: shape}
		for _, pl := range at {
			set, ok := all[pl.name]
			if !ok {
				continue // the profile has no such vocabulary (item_reason on module 3)
			}
			if c.Vocab == nil {
				c.Vocab, c.VocabAt = map[string][]string{}, map[string]string{}
			}
			c.Vocab[pl.name] = slices.Clone(set) // one copy per contract: editing one entry edits no other
			c.VocabAt[pl.name] = pl.path
		}
		return c
	}
	state := place{vocabJobState, PathState}
	errCode := place{vocabErrorCodes, PathErrorCode}
	page := []place{{vocabJobState, PathItemState}, {vocabErrorClass, PathItemErrorClass}, {vocabItemReason, PathItemReason}}
	cs := []modclient.RouteContract{
		rc(RouteQuote, "ok", "quote.ok.json", 200, sh.quote),

		rc(RouteRun, "created", "run.created.json", 202, sh.run, state),
		rc(RouteRun, "replay", "run.replay.json", 200, sh.run, state),
		rc(RouteRun, "error validation_failed", "run.error.validation_failed.json", 400, sh.errBody, errCode),
		rc(RouteRun, "error unauthenticated", "run.error.unauthenticated.json", 401, sh.errBody, errCode),
		rc(RouteRun, "error not_found", "run.error.not_found.json", 404, sh.errBody, errCode),
		rc(RouteRun, "error conflict", "run.error.conflict.json", 409, sh.conflict, errCode, place{vocabRunConflictCause, PathConflictCause}),
		rc(RouteRun, "error paused", "run.error.paused.json", 503, sh.errBody, errCode),
		rc(RouteRun, "error internal_error", "run.error.internal_error.json", 500, sh.errBody, errCode),

		rc(RouteReport, "job sent", "report.job.sent.json", 200, sh.job, state),
		rc(RouteReport, "job queued", "report.job.queued.json", 200, sh.job, state),
		rc(RouteReport, "job error not_found", "report.job.error.not_found.json", 404, sh.errBody, errCode),
		rc(RouteReport, "campaign page", "report.campaign.page.json", 200, sh.page, page...),
		rc(RouteReport, "campaign last page", "report.campaign.last_page.json", 200, sh.page, place{vocabJobState, PathItemState}),
		rc(RouteReport, "campaign empty", "report.campaign.empty.json", 200, sh.page),
		rc(RouteReport, "campaign error cursor_malformed", "report.campaign.error.cursor_malformed.json", 400, sh.errBody, errCode),

		rc(RouteDescribe, "ok", "describe.ok.json", 200, sh.describe, place{vocabErrorCodes, PathDescribeErrorCode}),
	}
	if p.Cancel {
		cs = append(cs,
			rc(RouteCancel, "ok", "cancel.ok.json", 200, sh.cancel),
			rc(RouteCancel, "error validation_failed", "cancel.error.validation_failed.json", 400, sh.errBody, errCode),
		)
	}
	return cs
}
