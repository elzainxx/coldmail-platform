package m3

import "slices"

// The closed sets this adapter maps from, each handled by ONE switch below and listed once here as the Vocab of the contracts that carry it. A test reads the switches' case literals from
// this file and holds them equal to these lists in both directions (a case the list does not name, a value the switch does not handle), with a paired control for each.
//
// TWO KINDS OF SET. The sets of the test button, of bulk add and of an account's verification and From verdict are user vocabularies the module has CLOSED (Phase 3 close-out C3: an internal
// reason is never serialised, and a value outside a set is mapped to the set's named fallback by the module itself): a word outside one of them is a Contract error here, never a guess, and
// nothing of it is carried (the switch answers ok = false and the caller names the key). The rest (a pause or quarantine reason, a quote reason, an event's actor and action) are
// descriptive words a user app shows and the module may add to; a word outside one of those is the Unrecognised word, which carries NO text from the module. The job state is strict and
// lives in sendapi (a new state decides what is charged), and so is the cause of a run refusal. `smtp_response` (the provider's scrubbed reply) and `message` (rendered copy) are free text by
// the module's own documentation: carried as opaque text, bounded, never branched on.
//
// The describe names are the module's own (describe.vocabularies.<name>) where the module publishes the set, so the live drift gate compares like with like; the others are named for the
// route.
const (
	vocabCredentialVerification = "credential_verification_state" // describe
	vocabFromVerification       = "from_verification_state"       // describe
	vocabQuoteReason            = "quote_reason_code"             // describe
	vocabPauseReason            = "pause_reason"                  // describe
	vocabQuarantineReason       = "quarantine_reason"             // describe
	vocabBulkRowStatus          = "bulk_add_row_status"           // describe
	vocabTestVerification       = "test_verification_state"       // the test button's and a bulk row's verification words
	vocabTestAlignment          = "test_alignment_state"          // the test button's and a bulk row's From verdict words
	vocabVerifyCode             = "verify_code"                   // the verify_* failure codes and the test button's two "could not run" codes
	vocabAlignmentCode          = "alignment_code"                // from_alignment.code of the test button: why the From check could not run
	vocabAlignmentError         = "alignment_error"               // from_alignment.error of the test button: the check ran and its result could not be saved
	vocabBulkRejectCode         = "bulk_reject_code"              // the rejected rows' codes
	// vocabBulkRowCode is a bulk row's `code` as a whole: a rejected row's reason or the reason a created row's credential check failed, which share the one position of the answer, so the
	// contract constrains the union of the two sets (the switches rejectReason and verifyCode, held equal to it by a test).
	vocabBulkRowCode = "bulk_row_code"
)

// sets is the one place the sets are written down; Vocab and the tests read them through set().
var sets = map[string][]string{
	vocabCredentialVerification: {"unverified", "verified", "failed"},
	vocabFromVerification:       {"unverified", "aligned", "unaligned_warned", "blocked"},
	vocabQuoteReason: {
		"dispatch_disabled", "exceeds_queue_cap", "no_eligible_accounts", "all_accounts_unverified", "from_gate_blocked", "suspended", "campaign_active", "campaign_cap",
	},
	vocabPauseReason:      {"daily_limit", "throttle", "manual"},
	vocabQuarantineReason: {"auth_failure", "consecutive_failures", "failure_rate", "manual"},
	vocabBulkRowStatus:    {"created", "rejected"},
	vocabTestVerification: {"verified", "failed", "unverified", "unavailable"},
	vocabTestAlignment:    {"unverified", "aligned", "unaligned_warned", "blocked", "indeterminate"},
	vocabVerifyCode: {
		"verify_connect_failed", "verify_timeout", "verify_tls_unavailable", "verify_tls_failed", "verify_auth_failed", "verify_protocol_error", "verify_connection_lost",
		"verify_host_not_allowed", "verify_error", "verification_unavailable",
	},
	vocabAlignmentCode:  {"from_dns_unavailable", "from_domain_not_found"},
	vocabAlignmentError: {"from_check_not_saved"},
	vocabBulkRejectCode: {
		"validation_failed", "account_cap_reached", "duplicate_account_for_user", "from_alignment_warning_unacknowledged", "conflict", "unprocessable", "not_found", "internal_error",
	},
}

func init() { sets[vocabBulkRowCode] = slices.Concat(sets[vocabBulkRejectCode], sets[vocabVerifyCode]) }

// set returns a copy of one closed set, for a contract's Vocab.
func set(name string) []string { return slices.Clone(sets[name]) }

// vocab returns the Vocab of a contract: the named sets, each a copy.
func vocab(names ...string) map[string][]string {
	if len(names) == 0 {
		return nil
	}
	v := make(map[string][]string, len(names))
	for _, n := range names {
		v[n] = set(n)
	}
	return v
}

// VerificationState is an account's credential verification state, or the test button's and a bulk row's (the closed set verified, failed, unverified, unavailable).
type VerificationState string

const (
	VerificationUnverified  VerificationState = "unverified"
	VerificationVerified    VerificationState = "verified"
	VerificationFailed      VerificationState = "failed"
	VerificationUnavailable VerificationState = "unavailable" // the test button and bulk add only: the check could not run
)

// verificationState maps an account's `verification.state` (the module's credential_verification_state enum: three words). A word outside it is not a state: ok is false.
func verificationState(s string) (VerificationState, bool) {
	switch s {
	case "unverified":
		return VerificationUnverified, true
	case "verified":
		return VerificationVerified, true
	case "failed":
		return VerificationFailed, true
	}
	return "", false
}

// testVerificationState maps the test button's `verification.state` and a bulk row's `verification` (the closed set of the two routes: the enum's three words and `unavailable`, which the
// route says when the check could not run). A word outside it is not a state: ok is false.
func testVerificationState(s string) (VerificationState, bool) {
	switch s {
	case "unverified":
		return VerificationUnverified, true
	case "verified":
		return VerificationVerified, true
	case "failed":
		return VerificationFailed, true
	case "unavailable":
		return VerificationUnavailable, true
	}
	return "", false
}

// AlignmentState is the From-gate's verdict on an account's from address.
type AlignmentState string

const (
	AlignmentUnverified AlignmentState = "unverified"
	AlignmentAligned    AlignmentState = "aligned"
	// AlignmentWarned is `unaligned_warned`: usable only once the user acknowledged the warning for the current verdict.
	AlignmentWarned  AlignmentState = "unaligned_warned"
	AlignmentBlocked AlignmentState = "blocked"
	// AlignmentIndeterminate is the test button's and a bulk row's word for "the DNS did not answer, nothing was decided and nothing was stored".
	AlignmentIndeterminate AlignmentState = "indeterminate"
)

// alignmentState maps an account's `from_alignment.state` (the module's from_verification_state enum: four words). A word outside it is not a state: ok is false.
func alignmentState(s string) (AlignmentState, bool) {
	switch s {
	case "unverified":
		return AlignmentUnverified, true
	case "aligned":
		return AlignmentAligned, true
	case "unaligned_warned":
		return AlignmentWarned, true
	case "blocked":
		return AlignmentBlocked, true
	}
	return "", false
}

// testAlignmentState maps the test button's `from_alignment.state` and a bulk row's `from_alignment_state` (the closed set of the two routes: the enum, and `indeterminate`). A word outside
// it is not a state: ok is false.
func testAlignmentState(s string) (AlignmentState, bool) {
	switch s {
	case "unverified":
		return AlignmentUnverified, true
	case "aligned":
		return AlignmentAligned, true
	case "unaligned_warned":
		return AlignmentWarned, true
	case "blocked":
		return AlignmentBlocked, true
	case "indeterminate":
		return AlignmentIndeterminate, true
	}
	return "", false
}

// AlignmentCode says why the test button's From check could not run (`from_alignment.code`, present with the state indeterminate).
type AlignmentCode string

const (
	AlignmentCodeNone AlignmentCode = ""
	// AlignmentDNSUnavailable: the DNS lookup did not answer.
	AlignmentDNSUnavailable AlignmentCode = "from_dns_unavailable"
	// AlignmentDomainNotFound: the From domain does not exist.
	AlignmentDomainNotFound AlignmentCode = "from_domain_not_found"
)

// alignmentCode maps `from_alignment.code`. "" is no code, not a code. A word outside the closed set is not a code: ok is false.
func alignmentCode(s string) (AlignmentCode, bool) {
	switch s {
	case "":
		return AlignmentCodeNone, true
	case "from_dns_unavailable":
		return AlignmentDNSUnavailable, true
	case "from_domain_not_found":
		return AlignmentDomainNotFound, true
	}
	return "", false
}

// alignmentError is the one word of `from_alignment.error`: the check ran and decided, and the verdict could not be saved (the internal slugs of the failed save went to the module's log).
// "" is no error. A word outside the closed set is not an error word: ok is false.
func alignmentError(s string) (notSaved, ok bool) {
	switch s {
	case "":
		return false, true
	case "from_check_not_saved":
		return true, true
	}
	return false, false
}

// VerifyCode says why a credential check did not verify. The eight verify_* codes are the module's closed failure set (dispatch/verify.go externalCodeFor, and since the Phase 3 close-out
// verify_host_not_allowed, the destination guard's refusal at connect time); the other two are what the test button says when it could not run the check at all.
type VerifyCode string

const (
	VerifyNone               VerifyCode = ""
	VerifyConnectFailed      VerifyCode = "verify_connect_failed"
	VerifyTimeout            VerifyCode = "verify_timeout"
	VerifyTLSUnavailable     VerifyCode = "verify_tls_unavailable"
	VerifyTLSFailed          VerifyCode = "verify_tls_failed"
	VerifyAuthFailed         VerifyCode = "verify_auth_failed"
	VerifyProtocolError      VerifyCode = "verify_protocol_error"
	VerifyConnectionLost     VerifyCode = "verify_connection_lost"
	VerifyHostNotAllowed     VerifyCode = "verify_host_not_allowed"
	VerifyError              VerifyCode = "verify_error"
	VerifyServiceUnavailable VerifyCode = "verification_unavailable"
)

// verifyCode maps a verification `code`. "" is no code, not a code. A word outside the closed set is not a code: ok is false.
func verifyCode(s string) (VerifyCode, bool) {
	switch s {
	case "":
		return VerifyNone, true
	case "verify_connect_failed":
		return VerifyConnectFailed, true
	case "verify_timeout":
		return VerifyTimeout, true
	case "verify_tls_unavailable":
		return VerifyTLSUnavailable, true
	case "verify_tls_failed":
		return VerifyTLSFailed, true
	case "verify_auth_failed":
		return VerifyAuthFailed, true
	case "verify_protocol_error":
		return VerifyProtocolError, true
	case "verify_connection_lost":
		return VerifyConnectionLost, true
	case "verify_host_not_allowed":
		return VerifyHostNotAllowed, true
	case "verify_error":
		return VerifyError, true
	case "verification_unavailable":
		return VerifyServiceUnavailable, true
	}
	return "", false
}

// RejectReason says why a bulk-add row was not created: the module's closed set (Phase 3 close-out C3). The first four are the codes the contract has always documented; `conflict`,
// `unprocessable`, `not_found` and `internal_error` are the external error codes that replaced a raw internal reason (`unique_violation:<constraint>`, `tx_begin_error`, a driver message,
// which now go to the module's log). A code outside the set is a Contract error.
type RejectReason string

const (
	RejectValidation       RejectReason = "validation_failed"
	RejectAccountCap       RejectReason = "account_cap_reached"
	RejectDuplicate        RejectReason = "duplicate_account_for_user"
	RejectAlignmentWarning RejectReason = "from_alignment_warning_unacknowledged"
	RejectConflict         RejectReason = "conflict"
	RejectUnprocessable    RejectReason = "unprocessable"
	RejectNotFound         RejectReason = "not_found"
	RejectInternal         RejectReason = "internal_error"
)

// rejectReason maps a rejected row's `code`. A code outside the closed set is not a reason: ok is false.
func rejectReason(s string) (RejectReason, bool) {
	switch s {
	case "validation_failed":
		return RejectValidation, true
	case "account_cap_reached":
		return RejectAccountCap, true
	case "duplicate_account_for_user":
		return RejectDuplicate, true
	case "from_alignment_warning_unacknowledged":
		return RejectAlignmentWarning, true
	case "conflict":
		return RejectConflict, true
	case "unprocessable":
		return RejectUnprocessable, true
	case "not_found":
		return RejectNotFound, true
	case "internal_error":
		return RejectInternal, true
	}
	return "", false
}

// InfeasibleCode is why a quote said a run cannot be accepted.
type InfeasibleCode string

const (
	InfeasibleDispatchDisabled      InfeasibleCode = "dispatch_disabled"
	InfeasibleExceedsQueueCap       InfeasibleCode = "exceeds_queue_cap"
	InfeasibleNoEligibleAccounts    InfeasibleCode = "no_eligible_accounts"
	InfeasibleAllAccountsUnverified InfeasibleCode = "all_accounts_unverified"
	InfeasibleFromGateBlocked       InfeasibleCode = "from_gate_blocked"
	// The three reasons that predict a run 409 beside exceeds_queue_cap (Phase 3 close-out C2); each carries the cause of the refusal it predicts (see InfeasibleReason.Cause).
	InfeasibleSuspended      InfeasibleCode = "suspended"
	InfeasibleCampaignActive InfeasibleCode = "campaign_active"
	InfeasibleCampaignCap    InfeasibleCode = "campaign_cap"
	// InfeasibleUnrecognised is a reason the adapter does not know. The quote is still infeasible: only the word for why is missing.
	InfeasibleUnrecognised InfeasibleCode = "unrecognised"
)

// quoteReason maps a quote's `reasons[].code`.
func quoteReason(s string) InfeasibleCode {
	switch s {
	case "dispatch_disabled":
		return InfeasibleDispatchDisabled
	case "exceeds_queue_cap":
		return InfeasibleExceedsQueueCap
	case "no_eligible_accounts":
		return InfeasibleNoEligibleAccounts
	case "all_accounts_unverified":
		return InfeasibleAllAccountsUnverified
	case "from_gate_blocked":
		return InfeasibleFromGateBlocked
	case "suspended":
		return InfeasibleSuspended
	case "campaign_active":
		return InfeasibleCampaignActive
	case "campaign_cap":
		return InfeasibleCampaignCap
	}
	return InfeasibleUnrecognised
}

// pauseReason and quarantineReason map the account status words. They carry no behaviour of their own: a user app shows them.
func pauseReason(s string) string {
	switch s {
	case "daily_limit", "throttle", "manual":
		return s
	}
	return wordUnrecognised
}

func quarantineReason(s string) string {
	switch s {
	case "auth_failure", "consecutive_failures", "failure_rate", "manual":
		return s
	}
	return wordUnrecognised
}

// wordUnrecognised is the one word for a status word the adapter does not know.
const wordUnrecognised = "unrecognised"

// bulkStatus maps a bulk row's `status`. A status that is neither is not a row outcome: the whole answer is refused (a row that is not known to be created is not created).
func bulkStatus(s string) (created, ok bool) {
	switch s {
	case "created":
		return true, true
	case "rejected":
		return false, true
	}
	return false, false
}

// describedSets are the closed sets of this file that module 3 also publishes in describe.vocabularies, under the same names: the ones CheckAccountVocabularies compares.
var describedSets = []string{vocabCredentialVerification, vocabFromVerification, vocabQuoteReason, vocabPauseReason, vocabQuarantineReason, vocabBulkRowStatus}
