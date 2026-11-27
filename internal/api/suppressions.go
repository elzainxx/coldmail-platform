package api

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/coldmail-platform/platform/internal/jobs"
	"github.com/coldmail-platform/platform/internal/suppress"
)

// THE SUPPRESSION ROUTES (Phase 6 A4; Docs/jobs-api.md, "Suppressions"): the user's own suppression list, which every future job of theirs leaves out. List and search it, add an address, remove one (the removal is
// recorded by the database: platform.suppression_removals) and import a CSV. Every route sits behind the one authentication of the API, and the user is the authenticated principal's and nothing else: the path, the
// query and the body name no user (a body or a query that tries is refused as an unknown member), and the store is only ever given the principal's id, so another user's list cannot be read, searched, written or
// removed from here, and removing an address that is only on another user's list answers exactly as removing one nobody suppressed.
//
// WHAT AN ANSWER NEVER HOLDS: an address only where it is the user's own entry (the list, an add) and never in an error: a refused address is told as a word of the closed vocabulary (suppress.Reasons), a line of an
// import as (line, word). STRICT, EXACTLY LIKE THE JOB ROUTES: a JSON body is one object, application/json, at most 64 KiB, no member the route does not name, nothing after it; the import is text/csv streamed
// with the same cap as a job's list (64 MiB) and the same row limit (PLATFORM_JOB_MAX_ROWS).

// SuppressionService is what the API needs of the suppression list. *suppress.Store satisfies it; the unit tests use a fake. Every method takes the acting user from the authenticated principal.
type SuppressionService interface {
	List(ctx context.Context, userID, query, cursor string, limit int) ([]suppress.Entry, string, error)
	Add(ctx context.Context, userID, email string) (entry suppress.Entry, created bool, err error)
	Remove(ctx context.Context, userID, email string) (bool, error)
	Import(ctx context.Context, userID string, body io.Reader, maxRows int) (suppress.ImportResult, error)
}

var _ SuppressionService = (*suppress.Store)(nil)

const suppressionsPath = "/v1/suppressions"

// suppressionPaths are the routes served only when the API was given a suppression list (Deps.Suppressions).
func isSuppressionRoute(pattern string) bool {
	return pattern == suppressionsPath || pattern == suppressionsPath+"/remove" || pattern == suppressionsPath+"/import"
}

// entryView is one suppression as the user sees it.
type entryView struct {
	Email     string `json:"email"`
	Source    string `json:"source"`
	CreatedAt string `json:"created_at"`
}

func viewEntry(e suppress.Entry) entryView {
	return entryView{Email: e.Email, Source: e.Source, CreatedAt: stamp(e.CreatedAt)}
}

// emailBody is the body of an add and of a remove. The member is a pointer so that a body without it (or with null) is told from one that holds an empty address.
type emailBody struct {
	Email *string `json:"email"`
}

// readEmail reads the one-member body of an add or a remove. It answers the failure itself.
func (a *API) readEmail(w http.ResponseWriter, r *http.Request) (string, bool) {
	if _, ok := a.query(w, r); !ok {
		return "", false
	}
	var body emailBody
	if !a.readJSON(w, r, maxSmallBody, smallBodyWindow, &body) {
		return "", false
	}
	if body.Email == nil {
		a.refuseRequest(w, r, http.StatusBadRequest, codeValidationFailed, "email is required")
		return "", false
	}
	return *body.Email, true
}

func (a *API) listSuppressions(w http.ResponseWriter, r *http.Request, p principal) {
	q, ok := a.query(w, r, "q", "cursor", "limit")
	if !ok {
		return
	}
	limit, _, ok := a.intParam(w, r, q, "limit", 1, suppress.MaxListLimit)
	if !ok {
		return
	}
	list, next, err := a.supp.List(r.Context(), p.UserID, q.Get("q"), q.Get("cursor"), int(limit))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := struct {
		Entries    []entryView `json:"entries"`
		NextCursor *string     `json:"next_cursor"`
	}{Entries: make([]entryView, 0, len(list)), NextCursor: strPtr(next)}
	for _, e := range list {
		out.Entries = append(out.Entries, viewEntry(e))
	}
	a.writeJSON(w, r, http.StatusOK, out)
}

func (a *API) addSuppression(w http.ResponseWriter, r *http.Request, p principal) {
	email, ok := a.readEmail(w, r)
	if !ok {
		return
	}
	e, created, err := a.supp.Add(r.Context(), p.UserID, email)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if created {
		a.writeJSON(w, r, http.StatusCreated, struct {
			Entry entryView `json:"entry"`
		}{viewEntry(e)})
		return
	}
	a.writeJSON(w, r, http.StatusOK, struct {
		Entry   entryView `json:"entry"`
		Already bool      `json:"already"`
	}{viewEntry(e), true})
}

func (a *API) removeSuppression(w http.ResponseWriter, r *http.Request, p principal) {
	email, ok := a.readEmail(w, r)
	if !ok {
		return
	}
	removed, err := a.supp.Remove(r.Context(), p.UserID, email)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusOK, struct {
		Removed bool `json:"removed"`
	}{removed})
}

// importHeaderReasons are the sentences a refused header is told in: the API's own, never a cell of the file.
var importHeaderReasons = map[string]string{
	suppress.HeaderNone:       "the CSV has no header row",
	suppress.HeaderNoEmail:    "the header has no email column",
	suppress.HeaderEmailTwice: "the header names the email column twice",
	suppress.HeaderUnreadable: "the header row is not valid CSV",
}

func (a *API) importSuppressions(w http.ResponseWriter, r *http.Request, p principal) {
	if !a.wantMedia(w, r, "text/csv") {
		return
	}
	if _, ok := a.query(w, r); !ok || a.tooBig(w, r, maxCreateBody) {
		return
	}
	defer extendReadDeadline(w, createBodyWindow)()
	res, err := a.supp.Import(r.Context(), p.UserID, http.MaxBytesReader(w, r.Body, maxCreateBody), a.svc.MaxRows())
	if err != nil {
		var head *suppress.ImportHeaderError
		var many *suppress.TooManyRowsError
		switch {
		case errors.As(err, &head):
			reason, known := importHeaderReasons[head.Reason]
			if !known {
				reason = "the CSV header is not usable"
			}
			a.refuseRequest(w, r, http.StatusBadRequest, codeValidationFailed, reason)
		case errors.As(err, &many):
			a.fail(w, r, &jobs.TooManyRowsError{Limit: many.Limit}) // the same answer as a job's list that is too long
		default:
			a.fail(w, r, err) // a body over its cap is the 413 of classify; anything else is the server's
		}
		return
	}
	type invalidLine struct {
		Line   int    `json:"line"`
		Reason string `json:"reason"`
	}
	out := struct {
		Added         int           `json:"added"`
		AlreadyThere  int           `json:"already_there"`
		Invalid       int           `json:"invalid"`
		InvalidSample []invalidLine `json:"invalid_sample"`
	}{Added: res.Added, AlreadyThere: res.AlreadyThere, Invalid: res.Invalid, InvalidSample: make([]invalidLine, 0, len(res.Sample))}
	for _, s := range res.Sample {
		out.InvalidSample = append(out.InvalidSample, invalidLine{Line: s.Line, Reason: s.Reason})
	}
	a.writeJSON(w, r, http.StatusOK, out)
}
