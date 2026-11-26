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
// TestM3Adapter_ContractEdgeCases (m3_adapter_e2e_edge_test.go, which runs before this file); these two cannot be (the test button's `unavailable` was the third and left this list when TestM3Adapter_TestButtonSaysUnavailableWhenTheCredentialCheckCannotRun began to make the module give that answer on purpose (a damaged stored credential), Phase 3 close-out D1; its fixture is a recording since the Phase 4 A3 record run 45706128441; the From-gate's 422 `error unprocessable` was the fourth and left this list when the adapter tests turned the gate on, Phase 3 close-out D2: TestM3Adapter_FromGatePassWarnBlock makes the module answer it).
var m3DriftNotElicited = map[string]string{
	"m3|GET /v1/send-byo/accounts/{id}/events|page": "the case `page` is a hand-written page with a NEXT CURSOR and a PAUSE event (pause_reason daily_limit), and no page of this harness has either: a listing is one page of at most 200 events (a cursor needs more) and no account is ever paused here. Populated listings ARE observed since Phase 3 close-out D3 (TestM3Adapter_AccountEventsOfAnEnforcedHealthAction: created, verified and an enforced quarantine; the shadow control's observation), but contractcheck.Match gives a live body to the fixture it differs from least, and every populated page this module gives is nearer to the case `last page` (a recording of the close-out D3 observation, internal/adapters/m3/testdata/contract/accounts.events.last_page.json: the same three events) or to `empty`, so this case stays unexercised; it would take pausing an account for its daily limit AND more events than the page size, which no test needs",
	"m3|POST /v1/run|error internal_error":          "module 3 answers run with 500 internal_error only when one of its own database statements fails (the job insert, the config read that does not fail open, the advisory lock) inside a request; the harness has one shared Postgres and no way to fail a statement of module 3 alone without taking the whole stack down, and module 3 is never modified (CLAUDE.md rule 5)",
}

// TestM3Adapter_ZContractDrift is the gate itself, and, before the verdict, the Phase 4 D1 steps of the pending contracts (contract_pause_m3adapter_test.go: pause, resume and a run of a job item the
// platform marked cancelled, against the real module 3). The contracts of those answers are declared by the adapter (m3.PauseContracts) and listed only once their recordings are installed and
// the switch (sendapi.RecordedPauseFixtures) is flipped: until then the step prints them in record mode (CONTRACT_RECORD=1) and in compare mode compares each with its fixture only when the file is
// installed, SKIPPING the whole step with a printed note (and the count at the end) when none is. The step runs in a subtest of its own, so that a failure of it can never hide the verdict of the
// session below, and its calls never reach the session (m3Drift): a call no listed contract declares would be an undeclared dependency there.
func TestM3Adapter_ZContractDrift(t *testing.T) {
	t.Run("pending_contracts_pause_resume_cancelled_item", m3PauseCases)
	m3Drift.Finish(t)
	m3cp.finish(t)
}
