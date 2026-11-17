package m1

// The closed vocabularies the adapter maps from. Each set exists twice on purpose: as a list (what Contracts() publishes in RouteContract.Vocab, for the live drift gate) and as the
// switch of a known* function (what the adapter's decoding actually accepts). TestVocabListsAreTheSwitches parses this file and fails when the two differ, in either direction, so
// a value cannot be added to one and forgotten in the other.
//
// MODULE-FORCED(M1-F3): module 1 has no describe, vocabulary or policy route (its API has six routes and none lists values), so unlike modules 2 and 3 the platform cannot ask the
// module what it may say; the sets are copied from its code (internal/vocab/vocab.go and signals.go, internal/api/api.go handleJobStatus, migration 0009) and a value outside them
// is a contract error, never a guess.

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

// vocabSets is every set by the name Contracts() publishes it under, as fresh copies (a caller that edits one changes nothing here).
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
