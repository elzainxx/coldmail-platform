package api

import (
	"context"
	"net/http"

	"github.com/coldmail-platform/platform/internal/entitlements"
	"github.com/coldmail-platform/platform/internal/money"
)

// THE ACCOUNT ROUTES (Phase 6 A3; Docs/jobs-api.md, "The account"): GET /v1/me and GET /v1/balances, what the app's shell shows a signed-in user about themselves. Both sit behind the same
// authentication as every route and read ONLY what belongs to the principal: the user id comes from the verified login and nowhere else (no path, query or body names a user), and every statement the
// service runs is scoped by it. Nothing here calls a module: the balances are the platform's own books, and module 3's state is what the platform last observed (the reconcile loop's reading), never a
// request made now.

// AccountService is what the two routes need besides the job store. The production wiring (internal/app) answers it from the tenant resolver, the money store and the entitlement store; the unit
// tests use a fake. Every method takes the acting user from the authenticated principal.
type AccountService interface {
	// Email is the user's address as the platform's user table holds it, read through the tenant resolver's sanctioned view (never a claim of the token).
	Email(ctx context.Context, userID string) (string, error)
	// Balance is the user's credits on a metered module (m1 or m2); a user who has none yet has zeros.
	Balance(ctx context.Context, userID, module string) (money.Balance, error)
	// Entitlement is the user's module 3 entitlement, with the database's own judgement of whether it is current (entitlements.Entitlement.Current).
	Entitlement(ctx context.Context, userID string) (entitlements.Entitlement, error)
	// M3Sync is the platform's bookkeeping of module 3's suspension state for the user, which holds the state module 3 was last SEEN in.
	M3Sync(ctx context.Context, userID string) (entitlements.SyncState, error)
}

// The paths of the three routes of this chunk. /v1/me and /v1/balances are mounted only when the API was given an AccountService (Deps.Account); the template check needs none.
const (
	mePath       = "/v1/me"
	balancesPath = "/v1/balances"
)

// meView is the answer of GET /v1/me. Members are only ever added.
type meView struct {
	Email string `json:"email"`
}

func (a *API) me(w http.ResponseWriter, r *http.Request, p principal) {
	if _, ok := a.query(w, r); !ok {
		return
	}
	email, err := a.account.Email(r.Context(), p.UserID)
	if err != nil {
		if why, refusal := resolveWhy(err); refusal {
			a.refuse(w, r, why) // the user was switched off, or removed, between the authentication of this request and this read: the login is refused, as at the door
			return
		}
		a.fail(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusOK, meView{Email: email})
}

// The states module 3 is reported in. They are the platform's own words for what it last SAW module 3 hold the user in (platform.m3_sync.observed): "unknown" is what it says before it has seen
// anything, and for anything it does not recognise.
const (
	m3Active    = "active"
	m3Suspended = "suspended"
	m3Unknown   = "unknown"
)

// m3State maps the observation the platform holds to the word of the answer. Only an observation that is exactly "active" or "suspended" is reported as such (CLAUDE.md rule 21: the platform never
// reports a module 3 state module 3 was not seen to be in); a user with no bookkeeping, "unknown" and any other text are "unknown". The job store's confirm reads the same column and refuses on "suspended".
func m3State(s entitlements.SyncState) string {
	if !s.Exists {
		return m3Unknown
	}
	switch s.Observed {
	case m3Active:
		return m3Active
	case m3Suspended:
		return m3Suspended
	}
	return m3Unknown
}

type balanceView struct {
	Available int64 `json:"available"`
	Reserved  int64 `json:"reserved"`
}

type m3View struct {
	Enabled bool    `json:"enabled"`
	Until   *string `json:"until"`
	State   string  `json:"state"`
}

// balancesView is the answer of GET /v1/balances: the credits of the two metered modules and module 3's entitlement. Members are only ever added.
type balancesView struct {
	Modules struct {
		M1 balanceView `json:"m1"`
		M2 balanceView `json:"m2"`
	} `json:"modules"`
	M3 m3View `json:"m3"`
}

func (a *API) balances(w http.ResponseWriter, r *http.Request, p principal) {
	if _, ok := a.query(w, r); !ok {
		return
	}
	var out balancesView
	for _, m := range []struct {
		module string
		into   *balanceView
	}{{"m1", &out.Modules.M1}, {"m2", &out.Modules.M2}} {
		b, err := a.account.Balance(r.Context(), p.UserID, m.module)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		*m.into = balanceView{Available: b.Available, Reserved: b.Reserved}
	}
	ent, err := a.account.Entitlement(r.Context(), p.UserID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	sync, err := a.account.M3Sync(r.Context(), p.UserID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out.M3 = m3View{Enabled: ent.Exists && ent.Current, Until: stampPtr(ent.Until), State: m3State(sync)}
	a.writeJSON(w, r, http.StatusOK, out)
}
