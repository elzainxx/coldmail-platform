package app

import (
	"context"

	"github.com/coldmail-platform/platform/internal/api"
	"github.com/coldmail-platform/platform/internal/entitlements"
	"github.com/coldmail-platform/platform/internal/money"
	"github.com/coldmail-platform/platform/internal/tenant"
)

// accountSource is what GET /v1/me and GET /v1/balances (internal/api, Phase 6 A3) read, answered from the three stores the process already has: the tenant resolver (the address, through the
// sanctioned view of the users), the money store (the balances of the two metered modules) and the entitlement store (module 3's entitlement and the bookkeeping of what the platform last saw module 3
// do). Each method only forwards: the rules (the user's id is the principal's, every statement is scoped by it, time is the database's) are in the stores, and nothing here calls a module.
type accountSource struct {
	tenants *tenant.Resolver
	money   *money.Store
	ents    *entitlements.Store
}

var _ api.AccountService = accountSource{}

// NewAccountService is the account service the process gives the API, for the one caller that builds the API the way the service does without running it: the harness's tests of the whole stack.
func NewAccountService(tenants *tenant.Resolver, mny *money.Store, ents *entitlements.Store) api.AccountService {
	return accountSource{tenants: tenants, money: mny, ents: ents}
}

func (a accountSource) Email(ctx context.Context, userID string) (string, error) {
	return a.tenants.Email(ctx, userID)
}

func (a accountSource) Balance(ctx context.Context, userID, module string) (money.Balance, error) {
	return a.money.Balance(ctx, userID, module)
}

func (a accountSource) Entitlement(ctx context.Context, userID string) (entitlements.Entitlement, error) {
	return a.ents.Get(ctx, userID)
}

func (a accountSource) M3Sync(ctx context.Context, userID string) (entitlements.SyncState, error) {
	return a.ents.Sync(ctx, userID)
}
