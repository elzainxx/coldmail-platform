//go:build harness && m1node

package harness

import (
	"testing"

	m1ad "github.com/coldmail-platform/platform/internal/adapters/m1"
	"github.com/coldmail-platform/platform/internal/contractcheck"
)

// Phase 3, chunk B6: the contract drift gate for module 1. Every response the REAL module 1 gave to an adapter built in this binary (the end-to-end tests' tenants and the node
// tests' live client) is compared with the recorded fixture of the contract it belongs to (internal/adapters/m1/testdata/contract): a key that went missing or appeared, a type
// that changed, a vocabulary value the adapter does not handle, or a status that changed fails the test naming the route, the case and the path. A response no contract declares
// fails (a dependency nobody wrote down); a contract no response was matched to fails unless it is listed below with the reason the harness cannot elicit it. The Session prints
// what it does not cover. This file is named zz_ so that its test is the LAST of the binary: Go runs a package's tests in the order of the files' names.
//
// m1Drift is created at package initialisation, before any test builds an adapter; m1bObserve (m1_bulk_settle_test.go) is the hook the untagged helpers read.
var m1Drift = contractcheck.NewSession(m1ad.Contracts(), m1ad.ContractFixtures(), "testdata/contract", m1DriftNotElicited)

// m1DriftNotElicited: module|route|case -> why the harness cannot make module 1 give that answer. Every entry needs a reason; an entry for a contract that WAS exercised is red
// (stale). Three entries since the record runs of the Phase 3 close-out: the three answers of the bulk route that need a fault inside module 1 or its kill switch pulled at boot, each with its reason.
var m1DriftNotElicited = map[string]string{
	"m1|POST /v1/verify/bulk|paused":         "module 1 answers 503 paused only while its front-door kill switch is pulled in reject mode (TM_FRONTDOOR_MODE=reject in the control plane's environment, read once at boot: api.go handleBulkSubmit, config KillSwitch.FrontDoorRejecting), so it takes a control plane started with that variable, and this stack's one control plane has to keep serving every other test of the binary and the outage sub-stage",
	"m1|POST /v1/verify/bulk|unavailable":    "module 1 answers 503 unavailable only when its bulk submitter or default policy is not wired (api.go handleBulkSubmit: s.bulk == nil || s.defPolicy == nil, that is a control plane started without its database, pepper or queue key), and this stack's control plane is always fully wired and waited on for readiness",
	"m1|POST /v1/verify/bulk|internal_error": "module 1 answers 500 internal_error only when the database write of a submission fails inside SubmitBulkRows (\"bulk submit: store write failed\"), which takes a fault in module 1's own database access that no honest call can cause and that no test should inject into the tables the whole binary shares",
}

func init() { m1bObserve = m1Drift.Observer() }

// TestM1Node_ZContractDrift is the gate itself (the last test of the binary), and, before the verdict, the Phase 4 D1 steps of the pending contracts (contract_cancel_m1node_test.go: the cancel of a
// running job, its repeats, a results page that holds a cancelled row and the cancel of an unknown job, against the real module 1). The contracts of those answers are declared by the adapter
// (m1.CancelContracts) and listed only once their recordings are installed and the switch (m1.recordedPauseFixtures) is flipped: until then the step prints them in record mode
// (CONTRACT_RECORD=1) and in compare mode compares each with its fixture only when the file is installed, SKIPPING the whole step with a printed note (and the count at the end) when none is. The
// step runs in a subtest of its own, so that a failure of it can never hide the verdict of the session below, and its calls never reach the session (m1Drift): a call no listed contract declares
// would be an undeclared dependency there.
func TestM1Node_ZContractDrift(t *testing.T) {
	t.Run("pending_contracts_cancel", m1CancelCases)
	m1Drift.Finish(t)
	m1cp.finish(t)
}
