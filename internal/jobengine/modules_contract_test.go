package jobengine_test

import (
	"testing"

	"github.com/coldmail-platform/platform/internal/jobengine/jobenginetest"
	"github.com/coldmail-platform/platform/internal/jobs"
)

// The fake of jobenginetest and the real wrappers over the adapters satisfy the SAME contract (jobenginetest.ModuleContract): item states to outcomes, an idempotent submit, an idempotent
// observe, no invented outcome, the error of the callback as it was, cancel, and the outage. The fake satisfies it in its own package; this runs it against Registry.For over the stand-ins of
// the three modules (modules_rig_test.go).
func TestModuleContract_TheRealWrappersOverTheAdaptersSatisfyItForEveryModule(t *testing.T) {
	jobenginetest.ModuleContract(t, []string{jobs.ModuleVerify, jobs.ModuleSendManaged, jobs.ModuleSendBYO}, realSubject)
}
