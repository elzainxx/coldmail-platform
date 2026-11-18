package m3

import (
	"slices"

	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// vocabs is the Vocab and the VocabAt of one contract: the closed sets the adapter maps from on that response and, for each, the path of the string-valued position of the response it
// constrains (modclient.RouteContract.VocabAt). A set is attached to a contract only when its recorded fixture HOLDS a string at that position: the live drift gate checks the strings the
// real module sends there against the set, and a set at a position the fixture leaves empty (a null pause reason, an array with no element) would constrain nothing in the fixture and
// could not be told from a mistyped path. The sets with no position in any fixture are the describe-published ones (pause_reason and quarantine_reason of an account), which
// CheckAccountVocabularies compares with the module's describe instead.
type vocabs struct {
	v  map[string][]string
	at map[string]string
}

// withSets builds a vocabs from name/path pairs: each name is a set of vocab.go (by name) and each path the position it constrains.
func withSets(pairs ...string) vocabs {
	if len(pairs)%2 != 0 {
		panic("m3: withSets takes name and path pairs")
	}
	if len(pairs) == 0 {
		return vocabs{}
	}
	out := vocabs{v: map[string][]string{}, at: map[string]string{}}
	for i := 0; i < len(pairs); i += 2 {
		out.v[pairs[i]] = set(pairs[i])
		out.at[pairs[i]] = pairs[i+1]
	}
	return out
}

// Contracts lists every module 3 response the adapter relies on: the shared routes and describe (sendapi.Contracts with module 3's profile, which has a cancel route and a 409 on run that
// names its causes: the cancel responses and the run conflict with its details come with it), module 3's feasibility quote, and the account routes (list, get, create, test, bulk add, events), each with the cases whose shape differs (success, an empty page, every error the adapter maps). Fixture names a file under
// this package's testdata/contract/ directory; the Shape of each entry is the one the adapter decodes that response with (the shared ones are built by the code the shared Client is, every
// other one is the package variable the decoding code uses). The fixtures are recordings from the real module except the ones testdata/README.md names (the hand-written: since the Phase 3 close-out fourteen, which the lead re-records from the real module).
//
// Vocab holds the closed sets the adapter maps from on that response, by describe's own names where the module publishes the set (vocab.go keeps each set equal to the switch that
// handles it), and VocabAt the path of the position each constrains (see vocabs). Error responses constrain the error code at error.code.
func Contracts() []modclient.RouteContract {
	codes := slices.Clone(Profile().ErrorCodes)
	errs := func() vocabs {
		return vocabs{v: map[string][]string{"error_codes": slices.Clone(codes)}, at: map[string]string{"error_codes": "error.code"}}
	}
	rc := func(route, name, fixture string, status int, shape modclient.Shape, v vocabs) modclient.RouteContract {
		return modclient.RouteContract{Module: modclient.M3, Route: route, Case: name, Fixture: fixture, Status: status, Shape: shape, Vocab: v.v, VocabAt: v.at}
	}
	// an account object (get, create): its credential verification and its From verdict; and one whose credential check failed also holds the reason
	account := withSets(vocabCredentialVerification, "verification.state", vocabFromVerification, "from_alignment.state")
	accountFailed := withSets(vocabCredentialVerification, "verification.state", vocabFromVerification, "from_alignment.state", vocabVerifyCode, "verification.code")
	// a listing of accounts holds them under items
	listed := withSets(vocabCredentialVerification, "items[].verification.state", vocabFromVerification, "items[].from_alignment.state")
	// the test button: its own two state words (a click that verified, one that failed and one that could not run differ in nothing else), and the reason of a check that did not verify
	test := withSets(vocabTestVerification, "verification.state", vocabTestAlignment, "from_alignment.state")
	testReason := withSets(vocabTestVerification, "verification.state", vocabTestAlignment, "from_alignment.state", vocabVerifyCode, "verification.code")
	// the click that could not run the check at all: a check that could not run says why (the From check's closed code)
	testUnavailable := withSets(vocabTestVerification, "verification.state", vocabTestAlignment, "from_alignment.state", vocabVerifyCode, "verification.code", vocabAlignmentCode, "from_alignment.code")
	eb := sendapi.ErrorShape()
	return append(sendapi.Contracts(Profile()),
		rc(RouteQuote, "feasible", "quote.feasible.json", 200, feasibilityShape, vocabs{}),
		rc(RouteQuote, "infeasible", "quote.infeasible.json", 200, feasibilityShape, withSets(vocabQuoteReason, "reasons[].code")),

		rc(RouteAccountsList, "page", "accounts.list.page.json", 200, accountListShape, listed),
		rc(RouteAccountsList, "last page", "accounts.list.last_page.json", 200, accountListShape, vocabs{}),
		rc(RouteAccountsList, "empty", "accounts.list.empty.json", 200, accountListShape, listed),
		rc(RouteAccountsList, "error validation_failed", "accounts.list.error.validation_failed.json", 400, eb, errs()),
		rc(RouteAccountsList, "error cursor_malformed", "accounts.list.error.cursor_malformed.json", 400, eb, errs()),

		rc(RouteAccountsGet, "ok", "accounts.get.ok.json", 200, accountShape, account),
		rc(RouteAccountsGet, "error not_found", "accounts.get.error.not_found.json", 404, eb, errs()),

		rc(RouteAccountsCreate, "created", "accounts.create.created.json", 201, accountShape, account),
		rc(RouteAccountsCreate, "created failed", "accounts.create.created_failed.json", 201, accountShape, accountFailed),
		rc(RouteAccountsCreate, "error validation_failed", "accounts.create.error.validation_failed.json", 400, eb, errs()),
		rc(RouteAccountsCreate, "error duplicate", "accounts.create.error.duplicate.json", 409, eb, errs()),
		rc(RouteAccountsCreate, "error conflict", "accounts.create.error.conflict.json", 409, eb, errs()),
		rc(RouteAccountsCreate, "error unprocessable", "accounts.create.error.unprocessable.json", 422, eb, errs()),

		rc(RouteAccountsTest, "ok", "accounts.test.ok.json", 200, testShape, test),
		rc(RouteAccountsTest, "failed", "accounts.test.failed.json", 200, testShape, testReason),
		rc(RouteAccountsTest, "unavailable", "accounts.test.unavailable.json", 200, testShape, testUnavailable),
		// the click whose credential check RAN (verified or failed, with its reason) but whose From check could not get an answer from DNS (Phase 3 close-out D2): the From check's state is indeterminate with the
		// closed code from_dns_unavailable. Its own case because the one above is the other way round (the credential check could not run), and a live body that has only the From half in common would tie with it.
		rc(RouteAccountsTest, "indeterminate", "accounts.test.indeterminate.json", 200, testShape, testUnavailable),
		rc(RouteAccountsTest, "error rate_limited", "accounts.test.error.rate_limited.json", 429, eb, errs()),
		rc(RouteAccountsTest, "error conflict", "accounts.test.error.conflict.json", 409, eb, errs()),
		rc(RouteAccountsTest, "error not_found", "accounts.test.error.not_found.json", 404, eb, errs()),

		// a bulk row's `code` is the module's reason for a rejection OR the reason a created account's credential check failed: one position, so one set, the union of the two
		rc(RouteAccountsBulk, "ok", "accounts.bulk.ok.json", 200, bulkShape,
			withSets(vocabBulkRowStatus, "results[].status", vocabTestVerification, "results[].verification", vocabTestAlignment, "results[].from_alignment_state", vocabBulkRowCode, "results[].code")),
		rc(RouteAccountsBulk, "error batch_too_large", "accounts.bulk.error.batch_too_large.json", 422, eb, errs()),
		rc(RouteAccountsBulk, "error conflict", "accounts.bulk.error.conflict.json", 409, eb, errs()),
		rc(RouteAccountsBulk, "error validation_failed", "accounts.bulk.error.validation_failed.json", 400, eb, errs()),

		rc(RouteAccountsEvents, "page", "accounts.events.page.json", 200, eventsShape, withSets(vocabPauseReason, "items[].to_flags.pause_reason")),
		// the page the module answers for an account that was created, verified and quarantined by an enforced health action (Phase 3 close-out D3): the last page of three events, no pause reason
		// anywhere and no next cursor. The matcher assigns a live body to the fixture it differs from least (contractcheck.Match: differences plus the positions that tell nothing on one side only), and
		// the case `page` above (a cursor, a pause reason) is further from every page this module gives than the empty one is, so a populated page of the harness lands here.
		rc(RouteAccountsEvents, "last page", "accounts.events.last_page.json", 200, eventsShape, vocabs{}),
		rc(RouteAccountsEvents, "empty", "accounts.events.empty.json", 200, eventsShape, vocabs{}),
	)
}
