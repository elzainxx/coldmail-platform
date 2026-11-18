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

// m3DriftNotElicited: module|route|case -> why the harness cannot make module 3 give that answer. Every other contract that the first record run did not reach is made to answer by
// TestM3Adapter_ContractEdgeCases (m3_adapter_e2e_edge_test.go, which runs before this file); these three cannot be.
var m3DriftNotElicited = map[string]string{
	"m3|POST /v1/run|error internal_error":                "module 3 answers run with 500 internal_error only when one of its own database statements fails (the job insert, the config read that does not fail open, the advisory lock) inside a request; the harness has one shared Postgres and no way to fail a statement of module 3 alone without taking the whole stack down, and module 3 is never modified (CLAUDE.md rule 5)",
	"m3|POST /v1/send-byo/accounts|error unprocessable":   "on account create module 3 answers 422 unprocessable only for an unacknowledged From-gate WARN verdict (account_create.go through fromGateVerdictSets): a WARN needs the gate on (run.sh stage 5b writes from_gate_mode=off) and real DNS answers for the From domain, and the harness network is internal with no DNS fixture (CoreDNS is not started), so even with the gate on it could only answer indeterminate, which creates the account anyway",
	"m3|POST /v1/send-byo/accounts/{id}/test|unavailable": "module 3 answers the test button with verification.state unavailable (code verify_error or verification_unavailable) only when its verifier is not configured (main.go always configures it) or Verify returns an error, a database write failing while the outcome is stored, which no sink mode causes; and this contract's body also carries from_alignment indeterminate / from_dns_unavailable, which needs the From-gate on with a DNS that does not answer",
}

// TestM3Adapter_ZContractDrift is the gate itself.
func TestM3Adapter_ZContractDrift(t *testing.T) { m3Drift.Finish(t) }
