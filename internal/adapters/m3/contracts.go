package m3

import (
	"slices"

	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// Contracts lists every module 3 response the adapter relies on: the shared four routes and describe (sendapi.Contracts with module 3's profile), module 3's feasibility quote, and the
// account routes (list, get, create, test, bulk add, events), each with the cases whose shape differs (success, an empty page, every error the adapter maps). Fixture names a file under
// this package's testdata/contract/ directory; the Shape of each entry is the one the adapter decodes that response with (the shared ones are built by the code the shared Client is, every
// other one is the package variable the decoding code uses). B6 replaces the hand-written bodies with recordings from the real module.
//
// Vocab holds the closed sets the adapter maps from on that response, by describe's own names where the module publishes the set (vocab.go keeps each set equal to the switch that
// handles it).
func Contracts() []modclient.RouteContract {
	codes := slices.Clone(Profile().ErrorCodes)
	errs := func() map[string][]string { return map[string][]string{"error_codes": slices.Clone(codes)} }
	account := func() map[string][]string {
		return vocab(vocabCredentialVerification, vocabFromVerification, vocabPauseReason, vocabQuarantineReason, vocabVerifyCode)
	}
	rc := func(route, name, fixture string, status int, shape modclient.Shape, v map[string][]string) modclient.RouteContract {
		return modclient.RouteContract{Module: modclient.M3, Route: route, Case: name, Fixture: fixture, Status: status, Shape: shape, Vocab: v}
	}
	eb := sendapi.ErrorShape()
	return append(sendapi.Contracts(Profile()),
		rc(RouteQuote, "feasible", "quote.feasible.json", 200, feasibilityShape, vocab(vocabQuoteReason)),
		rc(RouteQuote, "infeasible", "quote.infeasible.json", 200, feasibilityShape, vocab(vocabQuoteReason)),

		rc(RouteAccountsList, "page", "accounts.list.page.json", 200, accountListShape, account()),
		rc(RouteAccountsList, "last page", "accounts.list.last_page.json", 200, accountListShape, account()),
		rc(RouteAccountsList, "empty", "accounts.list.empty.json", 200, accountListShape, account()),
		rc(RouteAccountsList, "error validation_failed", "accounts.list.error.validation_failed.json", 400, eb, errs()),
		rc(RouteAccountsList, "error cursor_malformed", "accounts.list.error.cursor_malformed.json", 400, eb, errs()),

		rc(RouteAccountsGet, "ok", "accounts.get.ok.json", 200, accountShape, account()),
		rc(RouteAccountsGet, "error not_found", "accounts.get.error.not_found.json", 404, eb, errs()),

		rc(RouteAccountsCreate, "created", "accounts.create.created.json", 201, accountShape, account()),
		rc(RouteAccountsCreate, "created failed", "accounts.create.created_failed.json", 201, accountShape, account()),
		rc(RouteAccountsCreate, "error validation_failed", "accounts.create.error.validation_failed.json", 400, eb, errs()),
		rc(RouteAccountsCreate, "error duplicate", "accounts.create.error.duplicate.json", 409, eb, errs()),
		rc(RouteAccountsCreate, "error conflict", "accounts.create.error.conflict.json", 409, eb, errs()),
		rc(RouteAccountsCreate, "error unprocessable", "accounts.create.error.unprocessable.json", 422, eb, errs()),

		rc(RouteAccountsTest, "ok", "accounts.test.ok.json", 200, testShape, vocab(vocabTestVerification, vocabTestAlignment, vocabVerifyCode)),
		rc(RouteAccountsTest, "failed", "accounts.test.failed.json", 200, testShape, vocab(vocabTestVerification, vocabTestAlignment, vocabVerifyCode)),
		rc(RouteAccountsTest, "unavailable", "accounts.test.unavailable.json", 200, testShape, vocab(vocabTestVerification, vocabTestAlignment, vocabVerifyCode)),
		rc(RouteAccountsTest, "error rate_limited", "accounts.test.error.rate_limited.json", 429, eb, errs()),
		rc(RouteAccountsTest, "error conflict", "accounts.test.error.conflict.json", 409, eb, errs()),
		rc(RouteAccountsTest, "error not_found", "accounts.test.error.not_found.json", 404, eb, errs()),

		rc(RouteAccountsBulk, "ok", "accounts.bulk.ok.json", 200, bulkShape,
			vocab(vocabBulkRowStatus, vocabCredentialVerification, vocabFromVerification, vocabVerifyCode, vocabBulkRejectCode)),
		rc(RouteAccountsBulk, "error batch_too_large", "accounts.bulk.error.batch_too_large.json", 422, eb, errs()),
		rc(RouteAccountsBulk, "error conflict", "accounts.bulk.error.conflict.json", 409, eb, errs()),
		rc(RouteAccountsBulk, "error validation_failed", "accounts.bulk.error.validation_failed.json", 400, eb, errs()),

		rc(RouteAccountsEvents, "page", "accounts.events.page.json", 200, eventsShape, vocab(vocabPauseReason)),
		rc(RouteAccountsEvents, "empty", "accounts.events.empty.json", 200, eventsShape, vocab(vocabPauseReason)),
	)
}
