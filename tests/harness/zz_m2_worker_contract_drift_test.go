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
// ONE entry since the Phase 3 close-out: module 2 answers a forged cursor with a 400 (B2, module 2 dc2b532), so the shared contract that pinned the old 500 (a cursor whose time is not a time) can no
// longer be elicited from module 2; the same shared contract is still elicited from module 3 (its own allowlist and edge test), which has not changed.
var m2DriftNotElicited = map[string]string{
	"m2|GET /v1/report|campaign error cursor_forged": "since module 2 dc2b532 (Phase 3 close-out B2) a cursor that decodes but is not a timestamp and an id is a 400 validation_failed (the cursor_malformed contract), no longer the 500 internal_error this contract records; the 500 is only a database failure now, which the harness cannot cause on this route",
}

// TestM2Worker_ZContractDrift is the gate itself.
func TestM2Worker_ZContractDrift(t *testing.T) { m2Drift.Finish(t) }
