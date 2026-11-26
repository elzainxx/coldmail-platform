package sendapi

import (
	"slices"
	"testing"

	"github.com/coldmail-platform/platform/internal/modclient"
)

// SetOptionalErrorCodesForTest replaces the list of optional error codes for the rest of the test (the real list is empty now: job_item_cancelled is required) and puts the real one back at the
// end. The tests of the package do not run in parallel, so no other test reads the list while it is replaced.
func SetOptionalErrorCodesForTest(t testing.TB, codes ...string) {
	t.Helper()
	old := optionalErrorCodes
	optionalErrorCodes = slices.Clone(codes)
	t.Cleanup(func() { optionalErrorCodes = old })
}

// This file is compiled only into the tests of the package: it hands the external tests (package sendapi_test) the few unexported things they must look at.

// StateClassForTest is the adapter's switch over item states.
func StateClassForTest(state string) (Class, bool) { return stateClass(state) }

// UnvalidatedClientForTest is a Client with a profile that was NOT validated by New and no module client: it can classify items and nothing else. It lets a test drive the paths a validated
// profile can never reach (a module the charge table does not name).
func UnvalidatedClientForTest(p Profile) *Client { return &Client{p: p.clone()} }

// ClientShapesForTest are the Shapes a client built by New decodes with, by the names the contracts use. The pause shape is among them only once a contract names it (recordedPauseFixtures): the
// test of it requires every shape it lists to be carried by a contract.
func ClientShapesForTest(c *Client) map[string]modclient.Shape {
	m := map[string]modclient.Shape{"quote": c.sh.quote, "run": c.sh.run, "job": c.sh.job, "page": c.sh.page, "describe": c.sh.describe, "error": c.sh.errBody, "cancel": c.sh.cancel, "conflict": c.sh.conflict}
	if recordedPauseFixtures {
		m["pause"] = c.sh.pause
	}
	return m
}

// PauseShapeForTest is the Shape the client decodes the answer of POST /v1/pause and POST /v1/resume with. It is among ClientShapesForTest only once the contracts name the answer (there is no
// recorded fixture before the switch is flipped), and that function's test requires every shape it lists to be carried by a contract.
func PauseShapeForTest(c *Client) modclient.Shape { return c.sh.pause }
