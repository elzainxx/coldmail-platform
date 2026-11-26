package sendapi

import "github.com/coldmail-platform/platform/internal/modclient"

// shapeSet is every Shape the client decodes a response with. It is built by newShapes from a Profile and nowhere else: New stores the set it builds, and Contracts builds the same set for the
// same profile, so a recorded body, the offline test and the production decoder cannot disagree about what the platform expects (a test compares the two sets).
type shapeSet struct {
	quote    modclient.Shape
	run      modclient.Shape
	job      modclient.Shape
	page     modclient.Shape
	describe modclient.Shape
	errBody  modclient.Shape
	// cancel is the cancel position; conflict is the body of a 409 on run (the generic error body for a module that says nothing more, the one with its details for a module that names its causes).
	cancel   modclient.Shape
	conflict modclient.Shape
	// pause is the answer of POST /v1/pause and POST /v1/resume (one shape for both): only campaign_id and paused are read; the rest of either module's body is tolerated and ignored.
	pause modclient.Shape
}

// errorShape is the module error envelope the platform relies on (modclient parses it; the Shape is what the drift gate compares). It is the same for every route and every module.
var errorShape = modclient.Object(modclient.Req("error", modclient.Object(
	modclient.Req("code", modclient.Str), modclient.Opt("message", modclient.Str), modclient.Opt("request_id", modclient.Str),
	modclient.Opt("details", modclient.ArrayOf(modclient.Object(modclient.Req("field", modclient.Str), modclient.Req("issue", modclient.Str)))),
)))

// ErrorShape is the Shape of a module error body, for an adapter that lists an error response of its own route among its contracts.
func ErrorShape() modclient.Shape { return errorShape }

func newShapes(p Profile) shapeSet {
	req, opt, str := modclient.Req, modclient.Opt, modclient.Str
	nullStr := str.OrNull()

	// quote: the keys both modules answer; module 3 adds the feasibility verdict.
	quote := []modclient.Field{
		req("recipient_count", modclient.Int), req("cost", modclient.Int), req("currency", str),
		req("unit_price_credits", modclient.Int), req("tier", str),
	}
	if p.QuoteFeasibility {
		// a reason is its code and, for the causes of a run refusal, the additive cause, the numbers and the cap name (Phase 3 close-out C2); the code is all the shared body reads
		quote = append(quote, req("feasible", modclient.Bool), req("reasons", modclient.ArrayOf(modclient.Object(
			req("code", str), opt("cause", str), opt("current", modclient.Int), opt("limit", modclient.Int), opt("requested", modclient.Int), opt("cap_name", str),
		))))
	}

	// The single-job report:
	// MODULE-FORCED(SA-F4): module 3's single report omits a key until it is set where module 2 sends it as null and a campaign item carries every key as null on both, so the Shape keeps the two conventions apart (a missing key must not read as null) and the single report's keys are required or optional by profile.
	single := func(name string, s modclient.Shape) modclient.Field {
		if p.AlwaysPresent {
			return req(name, s)
		}
		return opt(name, s)
	}
	job := []modclient.Field{req("job_id", str), req("state", str), req("attempt_count", modclient.Int)}
	job = append(job, single("error_class", nullStr), opt("provider_message_id", nullStr), opt("completed_at", nullStr))
	if p.hasReason() {
		job = append(job, single("reason", nullStr))
	}
	if p.AccountDetail {
		job = append(job, opt("smtp_response", nullStr), opt("failed_at", nullStr), opt("account_id", nullStr), opt("account_username", nullStr))
	}

	item := []modclient.Field{
		req("job_item_id", str), req("state", str), req("provider_message_id", nullStr), req("completed_at", nullStr),
		req("attempt_count", modclient.Int), req("error_class", nullStr),
	}
	if p.hasReason() {
		item = append(item, req("reason", nullStr))
	}
	if p.AccountDetail {
		item = append(item, req("smtp_response", nullStr), req("failed_at", nullStr), req("account_id", nullStr), req("account_username", nullStr))
	}

	totals := []modclient.Field{
		req("total", modclient.Int), req("sent", modclient.Int), req("bounced", modclient.Int), req("failed", modclient.Int),
		req("in_flight", modclient.Int), req("pending", modclient.Int), req("waiting_for_capacity", modclient.Int),
	}
	if p.hasCancelled() {
		totals = append(totals, req("cancelled", modclient.Int))
	}
	page := []modclient.Field{
		req("campaign_id", str),
		req("dispatch_paused", modclient.Bool),
		// ADDITIVE (Phase 4 C1): THIS campaign's own pause, not the operator's dispatch rung. Absent in a module older than C1 (and in every recorded fixture of the old modules): false.
		opt("paused", modclient.Bool),
		req("totals", modclient.Object(totals...)),
		req("progress", modclient.Object(req("terminal", modclient.Int), req("fraction_complete", modclient.Num), req("is_complete", modclient.Bool))),
		req("items", modclient.Object(
			req("results", modclient.ArrayOf(modclient.Object(item...))),
			req("next_cursor", nullStr),
			req("page_size", modclient.Int),
		)),
	}
	if p.AccountDetail {
		page = append(page, req("totals_by_account", modclient.ArrayOf(modclient.Object(
			req("account_id", str), req("account_username", str), req("sent", modclient.Int), req("bounced", modclient.Int), req("failed", modclient.Int), req("in_flight", modclient.Int),
		))))
	}

	// a 409 on run: module 2 says nothing but the generic envelope; module 3 names its causes in details[], one entry per cause ({field, issue} always; code, and for the two caps the numbers current and
	// limit, only some entries), and at this pin always names at least one
	conflict := errorShape
	if p.RunConflict == ConflictCauses {
		conflict = modclient.Object(modclient.Req("error", modclient.Object(
			modclient.Req("code", str), modclient.Opt("message", str), modclient.Opt("request_id", str),
			modclient.Req("details", modclient.ArrayOf(modclient.Object(req("field", str), req("issue", str), opt("code", str), opt("current", modclient.Int), opt("limit", modclient.Int)))),
		)))
	}

	return shapeSet{
		quote: modclient.Object(quote...),
		run:   modclient.Object(req("job_id", str), req("state", str)),
		job:   modclient.Object(job...),
		page:  modclient.Object(page...),
		// What modclient.Describe demands of a describe (its own copy is unexported): TestDescribeShapeAgreesWithModclient holds the two together.
		describe: modclient.Object(
			req("module", str), req("api_version", str), req("capabilities", modclient.MapOf(modclient.Bool)),
			req("pricing", modclient.Object(req("currency", str), req("model", str), req("unit_price_credits", modclient.Int), opt("models", modclient.ArrayOf(str)), opt("cost_fields_inert", modclient.Bool))),
			req("vocabularies", modclient.MapOf(modclient.Object(opt("values", modclient.ArrayOf(str))))),
			req("error_codes", modclient.ArrayOf(modclient.Object(req("code", str), req("http_status", modclient.Int)))),
		),
		errBody:  errorShape,
		cancel:   modclient.Object(req("campaign_id", str), req("cancelled", modclient.Int), req("in_flight", modclient.Int), req("already_terminal", modclient.Int)),
		conflict: conflict,
		// module 2 answers {campaign_id, paused, changed, state, queued, in_flight, terminal}, module 3 {campaign_id, state, paused}: the two required keys are the ones both carry and the only ones the
		// adapter relies on (the `state` words and module 2's `changed` are never read), and the rest is an open bag so that the two bodies are one contract.
		pause: modclient.Object(req("campaign_id", str), req("paused", modclient.Bool)).Open(),
	}
}
