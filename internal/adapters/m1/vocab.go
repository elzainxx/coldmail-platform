package m1

// The closed vocabularies the adapter maps from. Each set exists twice on purpose: as a list (what Contracts() publishes in RouteContract.Vocab, for the live drift gate, and what
// CheckVocabularies compares with the module's own describe) and as the switch of a known* function (what the adapter's decoding actually accepts). TestVocabListsAreTheSwitches
// parses this file and fails when the two differ, in either direction, so a value cannot be added to one and forgotten in the other.
//
// Module 1 publishes these sets itself since the Phase 3 close-out (GET /v1/describe, vocabularies{}: built from the module's own constants, internal/vocab), and the adapter asks
// it exactly as it asks modules 2 and 3 (Adapter.CheckVocabularies). The constants here are only the set the adapter HANDLES: a value outside them is a contract error at decode
// time, never a guess, and a describe that lists one the adapter does not know is a break found before a live row meets it.

var (
	// statusValues is the verdict status (vocab.go Status). Exactly four, frozen.
	statusValues = []string{"valid", "invalid", "risky", "unknown"}
	// reasonValues is the verdict reason (vocab.go Reason): 27 values, additive-only in the module.
	reasonValues = []string{
		"invalid_syntax", "invalid_domain", "no_mx", "null_mx", "dns_error", "disposable", "blacklisted_domain", "blacklisted_mx", "spam_trap", "role_account",
		"whitelisted", "provider_no_smtp", "plausible", "unknown_error", "accepted", "mailbox_not_found", "mailbox_full", "catch_all", "accept_all", "greylisted",
		"smtp_blocked", "smtp_timeout", "smtp_error", "connection_failed", "external_verified", "historical_bounce", "non_public_mx",
	}
	// originValues is signals.failure_origin (signals.go FailureOrigin): who failed when an answer could not be produced.
	originValues = []string{"own", "remote", "undetermined"}
	// skipValues is signals.smtp_skipped_reason (signals.go Skip*): why the SMTP stage did not run.
	skipValues = []string{"no_eligible_node", "audit_red", "provider_mx_routed", "warmup_cap", "rate_limited", "smtp_unwired"}
	// recommendationValues is the verdict recommendation (vocab.go Recommendation), read by Verify only.
	recommendationValues = []string{"send", "review", "skip"}
	// jobStatusValues is GET /v1/jobs/{id} `status`: the four words of the database CHECK (migration 0009), which the handler passes through. The code writes only running today and
	// derives complete; the adapter branches on the boolean `complete`, never on this word.
	jobStatusValues = []string{"submitted", "running", "complete", "complete_with_unknowns"}
	// submitStatusValues is the 202's `status`.
	submitStatusValues = []string{"accepted"}
)

// The two NON-VERDICT words (Phase 4 close-out C2, module 1 migration 0020): a row cancelled before its check began is served in the verdict object's shape with status `cancelled` and
// reason `cancelled_by_caller`. They are words of module 1's own vocabularies `non_verdict_status` and `non_verdict_reason`, NOT members of `status` and `reason` (a verdict is never
// cancelled), so they are kept apart from the seven sets above: the seven are what CheckVocabularies compares with the describe of the module, and these two are not (an older module has no
// such sets; a describe that lists them shows as an additive extra, never a break). TestNonVerdictVocabListsAreTheSwitches holds each list equal to its switch, as for the seven.
const (
	// StatusCancelled is the status of a result row that was cancelled before its check began (free, no verdict).
	StatusCancelled = "cancelled"
	// ReasonCancelledByCaller is the only reason that status carries.
	ReasonCancelledByCaller = "cancelled_by_caller"
)

var (
	// nonVerdictStatusValues is the module's `non_verdict_status`: exactly one word.
	nonVerdictStatusValues = []string{"cancelled"}
	// nonVerdictReasonValues is the module's `non_verdict_reason`: exactly one word.
	nonVerdictReasonValues = []string{"cancelled_by_caller"}
)

func knownNonVerdictStatus(s string) bool {
	switch s {
	case "cancelled":
		return true
	}
	return false
}

func knownNonVerdictReason(s string) bool {
	switch s {
	case "cancelled_by_caller":
		return true
	}
	return false
}

// cancelOutcomeValues is the `cancel_outcome` of POST /v1/jobs/{id}/cancel (ADR-0023): the three words, closed.
var cancelOutcomeValues = []string{CancelOutcomeCancelled, CancelOutcomeAlreadyCancelled, CancelOutcomeAlreadyComplete}

func knownCancelOutcome(s string) bool {
	switch s {
	case "cancelled", "already_cancelled", "already_complete":
		return true
	}
	return false
}

func knownStatus(s string) bool {
	switch s {
	case "valid", "invalid", "risky", "unknown":
		return true
	}
	return false
}

func knownReason(s string) bool {
	switch s {
	case "invalid_syntax", "invalid_domain", "no_mx", "null_mx", "dns_error", "disposable", "blacklisted_domain", "blacklisted_mx", "spam_trap", "role_account",
		"whitelisted", "provider_no_smtp", "plausible", "unknown_error", "accepted", "mailbox_not_found", "mailbox_full", "catch_all", "accept_all", "greylisted",
		"smtp_blocked", "smtp_timeout", "smtp_error", "connection_failed", "external_verified", "historical_bounce", "non_public_mx":
		return true
	}
	return false
}

func knownOrigin(s string) bool {
	switch s {
	case "own", "remote", "undetermined":
		return true
	}
	return false
}

func knownSkipToken(s string) bool {
	switch s {
	case "no_eligible_node", "audit_red", "provider_mx_routed", "warmup_cap", "rate_limited", "smtp_unwired":
		return true
	}
	return false
}

func knownRecommendation(s string) bool {
	switch s {
	case "send", "review", "skip":
		return true
	}
	return false
}

func knownJobStatus(s string) bool {
	switch s {
	case "submitted", "running", "complete", "complete_with_unknowns":
		return true
	}
	return false
}

// completeJobStatus says whether a job status word is one of the two that mean the job is finished: the handler turns a stored `running` into `complete` once every work item is
// terminal, so a response that says complete:true with any other word is the module contradicting itself.
func completeJobStatus(s string) bool { return s == "complete" || s == "complete_with_unknowns" }

func knownSubmitStatus(s string) bool {
	switch s {
	case "accepted":
		return true
	}
	return false
}

// vocabOrder is the order CheckVocabularies reports the sets in, by the names module 1's describe publishes them under.
var vocabOrder = []string{"status", "reason", "recommendation", "failure_origin", "smtp_skipped_reason", "job_status", "submit_status"}

// vocabSets is every set by the name Contracts() publishes it under (the name module 1's describe uses), as fresh copies (a caller that edits one changes nothing here).
func vocabSets() map[string][]string {
	return map[string][]string{
		"status":              append([]string(nil), statusValues...),
		"reason":              append([]string(nil), reasonValues...),
		"failure_origin":      append([]string(nil), originValues...),
		"smtp_skipped_reason": append([]string(nil), skipValues...),
		"recommendation":      append([]string(nil), recommendationValues...),
		"job_status":          append([]string(nil), jobStatusValues...),
		"submit_status":       append([]string(nil), submitStatusValues...),
	}
}
