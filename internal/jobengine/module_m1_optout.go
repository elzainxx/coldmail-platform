package jobengine

import (
	"context"
	"errors"

	"github.com/coldmail-platform/platform/internal/adapters/m1"
	"github.com/coldmail-platform/platform/internal/jobs"
)

// THE POLICY OF A MODULE 1 JOB (Phase 5 S6; module 1's ADR-0024). An opted-out job (jobs.Settings.VendorOptOut, frozen when the job was created) is submitted to module 1 under module 1's own opt-out policy, the
// default's twin without the `external` block, and every result the engine reads for it is checked to say it was made under that policy. The key is READ, from module 1's describe through
// RegistryDeps.OptOutPolicy: the platform holds no constant for it. An opted-in job is exactly what it was: no policy is sent and none is checked.
//
// NEVER A FALL-THROUGH. If the key cannot be had (no source, module 1 cannot be asked, an older module 1 that publishes none), the step WAITS (ErrWait: free, nothing sent, nothing recorded; the job goes
// stale and, if it never starts, expires at its start deadline like any job a module never took). A job is never submitted without the policy it was created for, because the only other policy module 1
// serves is the one that can reach the vendor.

// policyOf is the policy an opted-out job runs under ("" for an opted-in job, which names none), or the error the step waits with. Every failure to read the key is ErrWait, whatever module 1 answered
// (phaseDescribe is never a refusal of the job): a Contract or Internal answer is counted and logged as an answer the platform does not understand, like every other.
func (m *m1Module) policyOf(ctx context.Context, j jobs.EngineJob, p phase) (string, error) {
	if !j.Settings.VendorOptOut {
		return "", nil
	}
	const why = ": the job is opted out of the vendor fallback, so nothing is sent or read until module 1's opt-out policy is known (it is never submitted under the default policy)"
	src := m.reg.optOut
	if src == nil {
		return "", Wait("m1 " + string(p) + why + "; this platform has no source for it")
	}
	key, err := src.M1OptOutPolicy(ctx)
	switch {
	case err == nil && m1.ValidPolicyKey(key):
		return key, nil
	case err == nil, errors.Is(err, jobs.ErrNoOptOutPolicy):
		return "", Wait("m1 " + string(p) + why + "; module 1 publishes none")
	}
	return "", m.reg.failed(ctx, jobs.ModuleVerify, phaseDescribe, err, false)
}
