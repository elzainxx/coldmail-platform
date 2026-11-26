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
	// RoutePause and RouteResume are the routes of a module with a per-campaign pause (Profile.PauseCapability): module 2 send_managed_0031 and module 3 Phase 4 C1 (Phase 4 D1).
	RoutePause  = "POST /v1/pause"
	RouteResume = "POST /v1/resume"
)

// recordedPauseFixtures is THE SWITCH of the Phase 4 D1 recordings. The answers of POST /v1/pause and POST /v1/resume and the 409 `job_item_cancelled` of run are contracts (PauseContracts), and the
// contract drift sessions of the harness record them in record mode and compare them in compare mode; but until the lead has installed the three recordings (PauseFixtureNames, in this
// package's testdata/contract and in module 2's and module 3's) a contract without a fixture would be red in every adapter's no-rot check, and a fixture written by hand would be a recording nobody
// made. So while this is false the three are DECLARED but not LISTED: Contracts leaves them out and UncoveredRoutes names the two routes, with the reason. The lead flips it, as the ONLY edit of the
// switch, after installing the files: TestPauseFixturesAreInstalledWhenTheSwitchIsOn is red when it is true and a file is missing, and TestPauseFixturesDoNotExistWhileTheSwitchIsOff is red when it is
// false and one of them is there, so neither half of the change can be made without the other. The same switch exists for module 1's cancel (m1.recordedPauseFixtures).
const recordedPauseFixtures = false

// RecordedPauseFixtures says whether the Phase 4 D1 recordings are installed and their contracts listed (the value of the switch).
func RecordedPauseFixtures() bool { return recordedPauseFixtures }

// PauseFixtureNames are the three files the switch is about (under testdata/contract of this package, module 2's and module 3's): the answer of a pause, the answer of a resume and the 409
// of a run of an item the platform marked cancelled. The two answers are one SHAPE each (module 2 {campaign_id, paused, changed, state, queued, in_flight, terminal}, module 3 {campaign_id, state,
// paused}: a repeat, a release and a refusal-free no-op have the same keys and types), so there is one contract per route and one file; which of the answers the recorder kept is in the file.
func PauseFixtureNames() []string {
	return []string{"pause.ok.json", "resume.ok.json", "run.error.job_item_cancelled.json"}
}

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
// THE PHASE 4 D1 RECORDINGS (recordedPauseFixtures, PauseContracts). While the switch is false they are not listed: the answers of POST /v1/pause and POST /v1/resume and the 409 `job_item_cancelled`
// of run have no recorded fixture yet (the recordings of the two modules at their new heads are made after the pin, and the names are the same for both modules). Their shapes exist
// (shapeSet.pause) and are tested with hand-written bodies (the HandWritten tests); the report's additive `paused` is already in the page Shape (optional: every recorded page of an older module still
// decodes). When the switch is true Contracts lists them after the cancel entries.
//
// Contracts returns nil for a profile that fails validation: a subject that is empty is red in every test that counts it.
func Contracts(p Profile) []modclient.RouteContract {
	if p.validate() != nil {
		return nil
	}
	sh := newShapes(p)
	rc := contractBuilder(p)
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
	if recordedPauseFixtures {
		cs = append(cs, PauseContracts(p)...)
	}
	return cs
}

// contractBuilder is the one way an entry is built: the module of the profile and, for each place, a copy of the profile's closed set of that name (a profile with no such set, item_reason on
// module 3, leaves the place out).
func contractBuilder(p Profile) func(route, name, fixture string, status int, shape modclient.Shape, at ...place) modclient.RouteContract {
	all := p.Vocabularies()
	return func(route, name, fixture string, status int, shape modclient.Shape, at ...place) modclient.RouteContract {
		c := modclient.RouteContract{Module: p.Module, Route: route, Case: name, Fixture: fixture, Status: status, Shape: shape}
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
}

// PauseContracts are the contracts of the Phase 4 D1 recordings, DECLARED whether or not they are listed (Contracts lists them when recordedPauseFixtures is true; the harness resolves the contracts
// of its steps here so that record mode can print a recording under the right name before the files exist): the 200 of POST /v1/pause and of POST /v1/resume (a profile with a pause capability;
// one shape per route, see PauseFixtureNames), and the 409 `job_item_cancelled` of run (every profile: both senders answer it), whose error code is listed in the vocabulary of error.code even for
// a profile that has not learned the code (the contract is about it). nil for a profile that fails validation.
func PauseContracts(p Profile) []modclient.RouteContract {
	if p.validate() != nil {
		return nil
	}
	sh := newShapes(p)
	rc := contractBuilder(p)
	var cs []modclient.RouteContract
	if p.hasPause() {
		cs = append(cs,
			rc(RoutePause, "ok", "pause.ok.json", 200, sh.pause),
			rc(RouteResume, "ok", "resume.ok.json", 200, sh.pause),
		)
	}
	cancelled := rc(RouteRun, "error job_item_cancelled", "run.error.job_item_cancelled.json", 409, sh.errBody, place{vocabErrorCodes, PathErrorCode})
	if !slices.Contains(cancelled.Vocab[vocabErrorCodes], CodeItemCancelled) {
		cancelled.Vocab[vocabErrorCodes] = append(cancelled.Vocab[vocabErrorCodes], CodeItemCancelled)
	}
	return append(cs, cancelled)
}

// PendingContracts are the contracts that are declared (PauseContracts) and not listed yet: all of them while recordedPauseFixtures is false, none once it is true. The harness's drift sessions
// compare these with a fixture only when its file exists, and print them in record mode.
func PendingContracts(p Profile) []modclient.RouteContract {
	if recordedPauseFixtures {
		return nil
	}
	return PauseContracts(p)
}

// UncoveredRoutes are the routes the shared client calls that Contracts(p) does not cover, each with why: the drift gate prints them so that "all contracts match" is never read as "every route
// is pinned". While recordedPauseFixtures is false they are POST /v1/pause and POST /v1/resume (for a profile with the pause capability); once it is true there are none. A test keeps this list and
// Contracts exactly complementary over the routes the client calls.
func UncoveredRoutes(p Profile) map[string]string {
	out := map[string]string{}
	if !recordedPauseFixtures && p.hasPause() {
		const why = "added to the modules after the recordings: no recorded answer yet (its tests use hand-written bodies written from api-contract.md, named HandWritten); promote it to a contract by installing the recording and flipping recordedPauseFixtures"
		out[RoutePause], out[RouteResume] = why, why
	}
	return out
}
