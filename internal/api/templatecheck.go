package api

import (
	"net/http"

	"github.com/coldmail-platform/platform/internal/jobs"
)

// POST /v1/templates/check (Phase 6 A3; Docs/jobs-api.md, "The template check"): the authoring screen asks, as the user writes, whether the subject, the HTML and the text are templates the platform will
// accept, and which merge fields they use. It runs the SAME functions the job save runs (jobs.CheckCampaign: the save's size and text rules and its template compiler, the {{unsubscribe_url}}-in-a-group
// rule included), so it cannot call a campaign fine that the save then refuses. It touches no database and no store and reads nothing of the user's: the three strings in, an answer out; it sits
// behind the authentication like every route (an anonymous visitor does not get a free template compiler), and takes no part of the principal.
//
// STATUS 200 EITHER WAY. A template with a syntax error is the answer the route exists to give, not a failed request: {"ok":false,"errors":[{"part","line","column","message"}]}. A request that is not
// valid as a request (a size or text rule of the save: a subject over 998 bytes or on two lines, a body over 1 MiB, bytes that are not text; a member the route does not know; a body that is not JSON)
// is the ordinary 400, 413 or 415 of every route. A part may be empty.

const templatesCheckPath = "/v1/templates/check"

// maxCheckBody caps the body of the check: two bodies at the save's 1 MiB each, written out as JSON (a quote or a line break takes two bytes, a control character up to six), and the subject. Larger is
// a 413 before a byte is read when the client says so, and after the cap when it does not.
const maxCheckBody int64 = 8 << 20

type checkBody struct {
	Subject string `json:"subject"`
	HTML    string `json:"html"`
	Text    string `json:"text"`
}

type checkFieldView struct {
	Name        string `json:"name"`
	HasFallback bool   `json:"has_fallback"`
}

type checkErrorView struct {
	Part    string `json:"part"`
	Line    int    `json:"line"`
	Column  int    `json:"column"`
	Message string `json:"message"`
}

func (a *API) templatesCheck(w http.ResponseWriter, r *http.Request, _ principal) {
	if _, ok := a.query(w, r); !ok {
		return
	}
	var body checkBody
	if !a.readJSON(w, r, maxCheckBody, createBodyWindow, &body) {
		return
	}
	res, err := jobs.CheckCampaign(jobs.Content{Subject: body.Subject, HTML: body.HTML, Text: body.Text})
	if err != nil {
		a.fail(w, r, err) // a size or text rule of the save: the 400 validation_failed it always is
		return
	}
	if len(res.Errors) > 0 {
		errs := make([]checkErrorView, len(res.Errors))
		for i, e := range res.Errors {
			errs[i] = checkErrorView{Part: safePart(e.Part), Line: e.Line, Column: e.Column, Message: safeMessage(e.Message)}
		}
		a.writeJSON(w, r, http.StatusOK, struct {
			OK     bool             `json:"ok"`
			Errors []checkErrorView `json:"errors"`
		}{false, errs})
		return
	}
	fields := make([]checkFieldView, len(res.Fields))
	for i, f := range res.Fields {
		fields[i] = checkFieldView{Name: f.Name, HasFallback: f.HasFallback}
	}
	a.writeJSON(w, r, http.StatusOK, struct {
		OK     bool             `json:"ok"`
		Fields []checkFieldView `json:"fields"`
	}{true, fields})
}
