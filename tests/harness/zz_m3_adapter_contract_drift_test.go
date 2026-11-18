//go:build harness

package harness

import (
	"testing"

	m3ad "github.com/coldmail-platform/platform/internal/adapters/m3"
	"github.com/coldmail-platform/platform/internal/contractcheck"
)

// Phase 3, chunk B6: the contract drift gate for module 3 (see zz_m1_node_contract_drift_test.go for the rules). Every response the real module 3 gave to a client built by the
// m3_adapter tests (m3_adapter_e2e_helpers_test.go puts m3Drift.Observer() into the options) is compared with the recorded fixture of its contract
// (internal/adapters/m3/testdata/contract). The file is named zz_ so that this test is the last of the binary.
var m3Drift = contractcheck.NewSession(m3ad.Contracts(), m3ad.ContractFixtures(), "testdata/contract", m3DriftNotElicited)

// m3DriftNotElicited: module|route|case -> why the harness cannot make module 3 give that answer. EMPTY until the first runner run says which contracts are not reached.
var m3DriftNotElicited = map[string]string{}

// TestM3Adapter_ZContractDrift is the gate itself.
func TestM3Adapter_ZContractDrift(t *testing.T) { m3Drift.Finish(t) }
