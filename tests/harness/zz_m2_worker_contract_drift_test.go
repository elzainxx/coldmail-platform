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

// m2DriftNotElicited: module|route|case -> why the harness cannot make module 2 give that answer. EMPTY until the first runner run says which contracts are not reached.
var m2DriftNotElicited = map[string]string{}

// TestM2Worker_ZContractDrift is the gate itself.
func TestM2Worker_ZContractDrift(t *testing.T) { m2Drift.Finish(t) }
