package m3

import "slices"

// The closed sets this adapter maps from, each handled by ONE switch below and listed once here as the Vocab of the contracts that carry it. A test reads the switches' case literals from
// this file and holds them equal to these lists in both directions (a case the list does not name, a value the switch does not handle), with a paired control for each.
//
// A value outside a set is never an error and never guessed at: it maps to the set's Unrecognised word, which carries NO text from the module (an account whose state the adapter does not
// know is still listed; what the adapter cannot do is say what that state means). The job state is the one set that is strict, and it lives in sendapi (a new state decides what is charged).
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
	vocabTestVerification       = "test_verification_state"       // the test button's own word set
	vocabTestAlignment          = "test_alignment_state"          // the test button's own word set
	vocabVerifyCode             = "verify_code"                   // the verify_* failure codes and the test button's two "unavailable" codes
	vocabBulkRejectCode         = "bulk_reject_code"              // the rejected rows' codes the adapter recognises
)

// sets is the one place the sets are written down; Vocab and the tests read them through set().
var sets = map[string][]string{
	vocabCredentialVerification: {"unverified", "verified", "failed"},
	vocabFromVerification:       {"unverified", "aligned", "unaligned_warned", "blocked"},
	vocabQuoteReason:            {"dispatch_disabled", "exceeds_queue_cap", "no_eligible_accounts", "all_accounts_unverified", "from_gate_blocked"},
	vocabPauseReason:            {"daily_limit", "throttle", "manual"},
	vocabQuarantineReason:       {"auth_failure", "consecutive_failures", "failure_rate", "manual"},
	vocabBulkRowStatus:          {"created", "rejected"},
	vocabTestVerification:       {"verified", "failed", "unavailable"},
	vocabTestAlignment:          {"unverified", "aligned", "unaligned_warned", "blocked", "indeterminate"},
	vocabVerifyCode: {
		"verify_connect_failed", "verify_timeout", "verify_tls_unavailable", "verify_tls_failed", "verify_auth_failed", "verify_protocol_error", "verify_connection_lost",
		"verify_error", "verification_unavailable",
	},
	vocabBulkRejectCode: {"validation_failed", "account_cap_reached", "duplicate_account_for_user", "from_alignment_warning_unacknowledged"},
}

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

// VerificationState is an account's credential verification state, or the test button's.
type VerificationState string

const (
	VerificationUnverified  VerificationState = "unverified"
	VerificationVerified    VerificationState = "verified"
	VerificationFailed      VerificationState = "failed"
	VerificationUnavailable VerificationState = "unavailable" // the test button only: the check could not run
	// VerificationUnrecognised is a state the adapter does not know. It says nothing about the credential.
	VerificationUnrecognised VerificationState = "unrecognised"
)

// verificationState maps an account's `verification.state` and a bulk row's `verification` (the module's credential_verification_state enum).
func verificationState(s string) VerificationState {
	switch s {
	case "unverified":
		return VerificationUnverified
	case "verified":
		return VerificationVerified
	case "failed":
		return VerificationFailed
	}
	return VerificationUnrecognised
}

// testVerificationState maps the test button's `verification.state`: the enum's `verified` and `failed`, and `unavailable`, which the button says when the check could not run.
func testVerificationState(s string) VerificationState {
	switch s {
	case "verified":
		return VerificationVerified
	case "failed":
		return VerificationFailed
	case "unavailable":
		return VerificationUnavailable
	}
	return VerificationUnrecognised
}

// AlignmentState is the From-gate's verdict on an account's from address.
type AlignmentState string

const (
	AlignmentUnverified AlignmentState = "unverified"
	AlignmentAligned    AlignmentState = "aligned"
	// AlignmentWarned is `unaligned_warned`: usable only once the user acknowledged the warning for the current verdict.
	AlignmentWarned  AlignmentState = "unaligned_warned"
	AlignmentBlocked AlignmentState = "blocked"
	// AlignmentIndeterminate is the test button's word for "the DNS did not answer, nothing was decided and nothing was stored".
	AlignmentIndeterminate AlignmentState = "indeterminate"
	// AlignmentUnrecognised is a state the adapter does not know.
	AlignmentUnrecognised AlignmentState = "unrecognised"
)

// alignmentState maps an account's `from_alignment.state` and a bulk row's `from_alignment_state` (the module's from_verification_state enum).
func alignmentState(s string) AlignmentState {
	switch s {
	case "unverified":
		return AlignmentUnverified
	case "aligned":
		return AlignmentAligned
	case "unaligned_warned":
		return AlignmentWarned
	case "blocked":
		return AlignmentBlocked
	}
	return AlignmentUnrecognised
}

// testAlignmentState maps the test button's `from_alignment.state`: the enum, and `indeterminate`.
func testAlignmentState(s string) AlignmentState {
	switch s {
	case "unverified":
		return AlignmentUnverified
	case "aligned":
		return AlignmentAligned
	case "unaligned_warned":
		return AlignmentWarned
	case "blocked":
		return AlignmentBlocked
	case "indeterminate":
		return AlignmentIndeterminate
	}
	return AlignmentUnrecognised
}

// VerifyCode says why a credential check did not verify. The seven verify_* codes are the module's closed failure set (dispatch/verify.go externalCodeFor); the other two are what the test
// button says when it could not run the check at all.
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
	VerifyError              VerifyCode = "verify_error"
	VerifyServiceUnavailable VerifyCode = "verification_unavailable"
	// VerifyUnrecognised is a code the adapter does not know; the module's text is not carried.
	VerifyUnrecognised VerifyCode = "unrecognised"
)

// verifyCode maps a verification `code`. "" is no code, not a code.
func verifyCode(s string) VerifyCode {
	switch s {
	case "":
		return VerifyNone
	case "verify_connect_failed":
		return VerifyConnectFailed
	case "verify_timeout":
		return VerifyTimeout
	case "verify_tls_unavailable":
		return VerifyTLSUnavailable
	case "verify_tls_failed":
		return VerifyTLSFailed
	case "verify_auth_failed":
		return VerifyAuthFailed
	case "verify_protocol_error":
		return VerifyProtocolError
	case "verify_connection_lost":
		return VerifyConnectionLost
	case "verify_error":
		return VerifyError
	case "verification_unavailable":
		return VerifyServiceUnavailable
	}
	return VerifyUnrecognised
}

// RejectReason says why a bulk-add row was not created. The closed set is the codes the module's own validation and create path emit as fixed words; everything else (an infrastructure
// failure's internal reason, which can carry error text) is RejectUnrecognised and its text is dropped.
type RejectReason string

const (
	RejectValidation       RejectReason = "validation_failed"
	RejectAccountCap       RejectReason = "account_cap_reached"
	RejectDuplicate        RejectReason = "duplicate_account_for_user"
	RejectAlignmentWarning RejectReason = "from_alignment_warning_unacknowledged"
	// RejectUnrecognised is every other code, an infrastructure failure's internal reason included. Nothing of that text is carried.
	RejectUnrecognised RejectReason = "unrecognised"
)

// rejectReason maps a rejected row's `code`.
func rejectReason(s string) RejectReason {
	switch s {
	case "validation_failed":
		return RejectValidation
	case "account_cap_reached":
		return RejectAccountCap
	case "duplicate_account_for_user":
		return RejectDuplicate
	case "from_alignment_warning_unacknowledged":
		return RejectAlignmentWarning
	}
	return RejectUnrecognised
}

// InfeasibleCode is why a quote said a run cannot be accepted.
type InfeasibleCode string

const (
	InfeasibleDispatchDisabled      InfeasibleCode = "dispatch_disabled"
	InfeasibleExceedsQueueCap       InfeasibleCode = "exceeds_queue_cap"
	InfeasibleNoEligibleAccounts    InfeasibleCode = "no_eligible_accounts"
	InfeasibleAllAccountsUnverified InfeasibleCode = "all_accounts_unverified"
	InfeasibleFromGateBlocked       InfeasibleCode = "from_gate_blocked"
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
