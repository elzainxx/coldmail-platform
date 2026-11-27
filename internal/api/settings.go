package api

import (
	"context"
	"net/http"

	"github.com/coldmail-platform/platform/internal/usersettings"
)

// THE SETTINGS ROUTES (Phase 5 S6; Docs/jobs-api.md, "Settings"): GET and PUT /v1/settings, the user's own settings, which today are one: whether they are opted out of module 1's paid vendor fallback
// (vendor_fallback_opt_out, default false: opted IN). Both sit behind the same authentication as every route, and the user is the authenticated principal's and nothing else: the path, the query and the
// body name no user (a body or a query that tries is refused as an unknown member), and the store is only ever given the principal's id.
//
// WHAT A CHANGE DOES. It changes what the user's NEXT module 1 jobs are created with. The setting is read once when a module 1 job is created and frozen in the job (settings.vendor_opt_out), so a job that
// exists, drafts included, keeps the value it was created under; a customer who wants the new value on a draft discards it and creates it again. Every change is in the platform's audit trail
// (platform.user_settings_history), written by the database.
//
// STRICT, EXACTLY LIKE THE JOB ROUTES: the body of a PUT is one JSON object (application/json, at most 64 KiB) with the one member, a JSON boolean. An unknown member, a member of another type, a missing
// member, null, a body that is not an object and data after the object are all 400, and nothing is written.

// SettingsService is what the API needs of the settings store. *usersettings.Store satisfies it; the unit tests use a fake. Every method takes the acting user from the authenticated principal.
type SettingsService interface {
	Get(ctx context.Context, userID string) (usersettings.Settings, error)
	Set(ctx context.Context, userID string, vendorFallbackOptOut bool) (usersettings.Settings, error)
}

var _ SettingsService = (*usersettings.Store)(nil)

// userSettingsView is the answer of both routes: the settings now in force. Members are only ever added.
type userSettingsView struct {
	VendorFallbackOptOut bool `json:"vendor_fallback_opt_out"`
}

// putSettingsBody is the body of PUT /v1/settings. The member is a pointer so that a body without it (or with null) is told from one that says false.
type putSettingsBody struct {
	VendorFallbackOptOut *bool `json:"vendor_fallback_opt_out"`
}

func (a *API) getSettings(w http.ResponseWriter, r *http.Request, p principal) {
	if _, ok := a.query(w, r); !ok {
		return
	}
	s, err := a.settings.Get(r.Context(), p.UserID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusOK, userSettingsView{VendorFallbackOptOut: s.VendorFallbackOptOut})
}

func (a *API) putSettings(w http.ResponseWriter, r *http.Request, p principal) {
	if _, ok := a.query(w, r); !ok {
		return
	}
	var body putSettingsBody
	if !a.readJSON(w, r, maxSmallBody, smallBodyWindow, &body) {
		return
	}
	if body.VendorFallbackOptOut == nil {
		a.refuseRequest(w, r, http.StatusBadRequest, codeValidationFailed, "vendor_fallback_opt_out is required: true or false")
		return
	}
	s, err := a.settings.Set(r.Context(), p.UserID, *body.VendorFallbackOptOut)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusOK, userSettingsView{VendorFallbackOptOut: s.VendorFallbackOptOut})
}
