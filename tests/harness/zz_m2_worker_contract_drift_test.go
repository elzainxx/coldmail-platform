//go:build harness && m2worker

package harness

import (
	"testing"

	m2ad "github.com/coldmail-platform/platform/internal/adapters/m2"
	"github.com/coldmail-platform/platform/internal/contractcheck"
)

// Phase 3, chunk B6: the contract drift gate for module 2 (see zz_m1_node_contract_drift_test.go for the rules). Every response the real module 2 gave to an adapter built in
// the m2worker tests (m2_worker_e2e_helpers_test.go puts m2Drift.Observer() into the client's options) is compared with the recorded fixture of its contract
// (internal/adapters/m2/testdata/contract). The file is named zz_ so that this test is the last of the binary.
var m2Drift = contractcheck.NewSession(m2ad.Contracts(), m2ad.ContractFixtures(), "testdata/contract", m2DriftNotElicited)

// m2DriftNotElicited: module|route|case -> why the harness cannot make module 2 give that answer. EMPTY ON PURPOSE (B6c): the ten contracts the first record run did not reach are each made by a call the real
// module answers in TestM2Worker_ContractEdgeCases (m2_worker_contract_edge_test.go): run validation_failed, unauthenticated, conflict, paused and internal_error, report job queued, report campaign cursor_malformed,
// describe ok and cancel validation_failed. An entry here for a contract that WAS exercised is itself red (stale), so none is kept "just in case".
//
// EMPTY ON PURPOSE again since Phase 4 A1: the one entry the Phase 3 close-out had to add (the shared contract campaign error cursor_forged, a 500 module 2 stopped giving with its B2) is gone with the contract,
// because no module answers a forged cursor with a 500 any more (module 3 answers the 400 too), so every contract of module 2 is elicited and none needs a reason.
var m2DriftNotElicited = map[string]string{}

// TestM2Worker_ZContractDrift is the gate itself, and, before the verdict, the Phase 4 D1 steps of the pending contracts (contract_pause_m2worker_test.go: pause, resume and a run of a job item the
// platform marked cancelled, against the real module 2). The contracts of those answers are declared by the adapter (m2.PauseContracts) and listed only once their recordings are installed and
// the switch (sendapi.RecordedPauseFixtures) is flipped: until then the step prints them in record mode (CONTRACT_RECORD=1) and in compare mode compares each with its fixture only when the file is
// installed, SKIPPING the whole step with a printed note (and the count at the end) when none is. The step runs in a subtest of its own, so that a failure of it can never hide the verdict of the
// session below, and its calls never reach the session (m2Drift): a call no listed contract declares would be an undeclared dependency there.
func TestM2Worker_ZContractDrift(t *testing.T) {
	t.Run("pending_contracts_pause_resume_cancelled_item", m2PauseCases)
	m2Drift.Finish(t)
	m2cp.finish(t)
}
