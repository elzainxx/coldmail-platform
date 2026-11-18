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
// TestM3Adapter_ContractEdgeCases (m3_adapter_e2e_edge_test.go, which runs before this file); these two cannot be (the test button's `unavailable` was the third and left this list when the host guard's connect-time refusal made the module answer a body of the same structure, Phase 3 close-out C4/D5: TestM3Adapter_SMTPHostGuard; its fixture stays hand-written until a record run takes the answer; the From-gate's 422 `error unprocessable` was the fourth and left this list when the adapter tests turned the gate on, Phase 3 close-out D2: TestM3Adapter_FromGatePassWarnBlock makes the module answer it).
var m3DriftNotElicited = map[string]string{
	"m3|GET /v1/send-byo/accounts/{id}/events|page": "the case `page` is a hand-written page with a NEXT CURSOR and a PAUSE event (pause_reason daily_limit), and no page of this harness has either: a listing is one page of at most 200 events (a cursor needs more) and no account is ever paused here. Populated listings ARE observed since Phase 3 close-out D3 (TestM3Adapter_AccountEventsOfAnEnforcedHealthAction: created, verified and an enforced quarantine; the shadow control's observation), but contractcheck.Match gives a live body to the fixture it differs from least, and every populated page this module gives is nearer to the case `last page` (hand-written, internal/adapters/m3/testdata/contract/accounts.events.last_page.json: the same three events) or to `empty`, so this case stays unexercised; it would take pausing an account for its daily limit AND more events than the page size, which no test needs",
	"m3|POST /v1/run|error internal_error":          "module 3 answers run with 500 internal_error only when one of its own database statements fails (the job insert, the config read that does not fail open, the advisory lock) inside a request; the harness has one shared Postgres and no way to fail a statement of module 3 alone without taking the whole stack down, and module 3 is never modified (CLAUDE.md rule 5)",
}

// TestM3Adapter_ZContractDrift is the gate itself.
func TestM3Adapter_ZContractDrift(t *testing.T) { m3Drift.Finish(t) }
