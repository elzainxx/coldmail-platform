package app

import (
	"context"
	"fmt"

	"github.com/coldmail-platform/platform/internal/adapters/m1"
	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/jobs"
	"github.com/coldmail-platform/platform/internal/tenant"
)

// THE PROVIDER OF MODULE 1'S VENDOR REACHABILITY (Phase 4, row C3). jobs.VendorReachability over the module 1 adapter's describe: the one object the job store reads before every module 1 preview and
// confirm and the reachability loop reads every minute, so there is one way of asking and one answer.
//
// WHO ASKS. Describe is a service-level call, not a customer's, but module 1 has no service credential: every call it serves carries a per-caller key whose row names a user (its migration 0010),
// and the legacy static token is refused on describe. The platform therefore keeps ONE caller of its own at module 1, the SERVICE CALLER: a reserved id that is never a customer (m1ServiceCallerID),
// whose key is derived from PLATFORM_M1_KEY_SECRET and PLATFORM_M1_KEY_VERSION exactly as a customer's is, and which is registered through the same privileged function (platform.provision_m1_caller,
// through tenant.M1CallerEnsurer: once per process, idempotent) the first time it is used, so a fresh install needs no operator step and a rotation of the key version re-registers it with the rest.
// The row can submit nothing a customer could see (it is nobody's), and the describe holds no secret and reads no database.

// m1ServiceCallerID is the id of the platform's own caller at module 1: a canonical lower-case uuid that is not the all-zero system user (the function refuses that) and not a customer (customers are
// the ids of their logins, random version-4 uuids; this one is version 4 with the variant bits of an id nobody generates). It is a constant on purpose: the harness's scenario program names the same
// id (tests/harness/jobscenario, which the scope's selftest holds equal to this line).
const m1ServiceCallerID = "00000000-0000-4000-8000-00000000d5c1"

// callerEnsurer is what the provider registers the service caller through: *tenant.M1CallerEnsurer.
type callerEnsurer interface {
	EnsureCaller(ctx context.Context, userID string) error
}

// m1Reachability reads module 1's describe as the service caller.
type m1Reachability struct {
	factory *m1.Factory
	ensure  callerEnsurer
	secret  config.Secret
	version int
}

var (
	_ jobs.VendorReachability = (*m1Reachability)(nil)
	_ jobs.OptOutPolicySource = (*m1Reachability)(nil)
)

// adapter registers the service caller (a no-op after the first success of the process), derives its key and builds the module 1 adapter that acts as it. Every failure is an error that carries no key, no
// digest and no address (the tenant and adapter errors are written so).
func (p *m1Reachability) adapter(ctx context.Context) (*m1.Adapter, error) {
	if err := p.ensure.EnsureCaller(ctx, m1ServiceCallerID); err != nil {
		return nil, fmt.Errorf("register the platform's module 1 service caller: %w", err)
	}
	key, err := tenant.DeriveM1KeyVersion(p.secret, p.version, m1ServiceCallerID)
	if err != nil {
		return nil, fmt.Errorf("derive the platform's module 1 service key: %w", err)
	}
	return p.factory.ForScope(tenant.Scope{M1Key: key})
}

// M1VendorReachable registers the service caller (a no-op after the first success of the process), derives its key, asks module 1's describe and returns what it says: known is false when the describe
// has no vendor_fallback (an older module 1). Every failure is an error with no value beside it, and none of them carries a key, a digest or an address (the tenant and adapter errors are written so).
func (p *m1Reachability) M1VendorReachable(ctx context.Context) (reachable, known bool, err error) {
	ad, err := p.adapter(ctx)
	if err != nil {
		return false, false, err
	}
	v, err := ad.VendorReachable(ctx)
	if err != nil {
		return false, false, err
	}
	return v.Reachable, v.Known, nil
}

// M1OptOutPolicy is the key of module 1's opt-out policy, from the same describe and as the same service caller (Phase 5 S6: vendor_fallback.opt_out_policy, which module 1 publishes whether or not its vendor
// is reachable). It returns a valid policy name, or an error: jobs.ErrNoOptOutPolicy when module 1 answered and publishes none (an older module 1), any other error when it could not be asked. It never
// returns ("", nil): a caller treats every error the same way (no key, no opted-out job), and only the log's word differs.
func (p *m1Reachability) M1OptOutPolicy(ctx context.Context) (string, error) {
	ad, err := p.adapter(ctx)
	if err != nil {
		return "", err
	}
	o, err := ad.OptOutPolicy(ctx)
	if err != nil {
		return "", err
	}
	if !o.Known {
		return "", jobs.ErrNoOptOutPolicy
	}
	return o.Key, nil
}

// newVendorProvider builds the provider, or nil when module 1 cannot be asked in this process (PLATFORM_M1_URL or PLATFORM_M1_KEY_SECRET unset): the job store then reads nothing and refuses nothing
// for it, and the loop does not run. The result is a nil INTERFACE in that case, never a typed nil (the store tests it against nil). It is a variable only so that a test of the whole service can put
// a fake in its place; nothing else assigns it.
var newVendorProvider = func(a moduleAdapters, res *tenant.Resolver, db tenant.FuncDB, cfg config.Config) jobs.VendorReachability {
	if a.M1 == nil || res == nil || cfg.M1KeySecret.Reveal() == "" {
		return nil
	}
	return &m1Reachability{factory: a.M1, ensure: tenant.NewM1CallerEnsurer(res, db), secret: cfg.M1KeySecret, version: cfg.M1KeyVersion}
}
