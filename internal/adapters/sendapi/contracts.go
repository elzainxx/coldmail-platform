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
)

// Contracts lists every module response the shared body relies on, for the profile's module: one entry per route and per response whose shape differs (success, the replay of run, an
// empty page, each error the adapter maps). The Shape of each entry is the Shape the client decodes that response with: it is built by the same newShapes that New uses (a test holds the two
// equal), so the offline tests, the live drift gate and the production decoder cannot disagree. Fixture names a file under the adapter package's testdata/contract/ directory: the
// adapter package (m2, m3) holds the recorded bodies; the names are the same for both modules.
//
// An error entry's Shape is the error envelope the platform relies on; the envelope itself is parsed by modclient (a 4xx whose body claims to be an envelope and is not is a Contract error),
// so what the adapter adds to an error is its Kind and Code, which the tests assert per entry.
//
// Contracts returns nil for a profile that fails validation: a subject that is empty is red in every test that counts it.
func Contracts(p Profile) []modclient.RouteContract {
	if p.validate() != nil {
		return nil
	}
	sh := newShapes(p)
	all := p.Vocabularies()
	pick := func(names ...string) map[string][]string {
		v := map[string][]string{}
		for _, n := range names {
			if s, ok := all[n]; ok {
				v[n] = slices.Clone(s) // one copy per contract: editing one entry edits no other
			}
		}
		return v
	}
	item := func() map[string][]string { return pick(vocabJobState, vocabErrorClass, vocabItemReason) }
	errs := func() map[string][]string { return pick(vocabErrorCodes) }
	mod := p.Module
	rc := func(route, name, fixture string, status int, shape modclient.Shape, vocab map[string][]string) modclient.RouteContract {
		return modclient.RouteContract{Module: mod, Route: route, Case: name, Fixture: fixture, Status: status, Shape: shape, Vocab: vocab}
	}
	return []modclient.RouteContract{
		rc(RouteQuote, "ok", "quote.ok.json", 200, sh.quote, nil),

		rc(RouteRun, "created", "run.created.json", 202, sh.run, pick(vocabJobState)),
		rc(RouteRun, "replay", "run.replay.json", 200, sh.run, pick(vocabJobState)),
		rc(RouteRun, "error validation_failed", "run.error.validation_failed.json", 400, sh.errBody, errs()),
		rc(RouteRun, "error unauthenticated", "run.error.unauthenticated.json", 401, sh.errBody, errs()),
		rc(RouteRun, "error not_found", "run.error.not_found.json", 404, sh.errBody, errs()),
		rc(RouteRun, "error conflict", "run.error.conflict.json", 409, sh.errBody, errs()),
		rc(RouteRun, "error paused", "run.error.paused.json", 503, sh.errBody, errs()),
		rc(RouteRun, "error internal_error", "run.error.internal_error.json", 500, sh.errBody, errs()),

		rc(RouteReport, "job sent", "report.job.sent.json", 200, sh.job, item()),
		rc(RouteReport, "job queued", "report.job.queued.json", 200, sh.job, item()),
		rc(RouteReport, "job error not_found", "report.job.error.not_found.json", 404, sh.errBody, errs()),
		rc(RouteReport, "campaign page", "report.campaign.page.json", 200, sh.page, item()),
		rc(RouteReport, "campaign last page", "report.campaign.last_page.json", 200, sh.page, item()),
		rc(RouteReport, "campaign empty", "report.campaign.empty.json", 200, sh.page, item()),
		rc(RouteReport, "campaign error cursor_malformed", "report.campaign.error.cursor_malformed.json", 400, sh.errBody, errs()),
		rc(RouteReport, "campaign error cursor_forged", "report.campaign.error.cursor_forged.json", 500, sh.errBody, errs()),

		rc(RouteDescribe, "ok", "describe.ok.json", 200, sh.describe, pick(vocabJobState, vocabErrorClass, vocabItemReason, vocabErrorCodes)),
	}
}
