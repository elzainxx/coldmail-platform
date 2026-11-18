package sendapi

import "github.com/coldmail-platform/platform/internal/modclient"

// This file is compiled only into the tests of the package: it hands the external tests (package sendapi_test) the few unexported things they must look at.

// StateClassForTest is the adapter's switch over item states.
func StateClassForTest(state string) (Class, bool) { return stateClass(state) }

// UnvalidatedClientForTest is a Client with a profile that was NOT validated by New and no module client: it can classify items and nothing else. It lets a test drive the paths a validated
// profile can never reach (a module the charge table does not name).
func UnvalidatedClientForTest(p Profile) *Client { return &Client{p: p.clone()} }

// ClientShapesForTest are the Shapes a client built by New decodes with, by the names the contracts use.
func ClientShapesForTest(c *Client) map[string]modclient.Shape {
	return map[string]modclient.Shape{"quote": c.sh.quote, "run": c.sh.run, "job": c.sh.job, "page": c.sh.page, "describe": c.sh.describe, "error": c.sh.errBody, "cancel": c.sh.cancel, "conflict": c.sh.conflict}
}
