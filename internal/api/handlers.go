package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/coldmail-platform/platform/internal/jobs"
)

// jobBody is the answer of every route that returns one job.
type jobBody struct {
	Job jobView `json:"job"`
}

// isJobID is the canonical lower-case uuid a job's id is. Another spelling is a job that does not exist (the store answers the same).
func isJobID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
				return false
			}
		}
	}
	return true
}

// jobID is the {id} of the path, or false after the 404 a job that cannot exist gets.
func (a *API) jobID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if !isJobID(id) {
		a.fail(w, r, jobs.ErrNotFound)
		return "", false
	}
	return id, true
}

// ---- reads ------------------------------------------------------------------------------------------------------

func (a *API) get(w http.ResponseWriter, r *http.Request, p principal) {
	id, ok := a.jobID(w, r)
	if !ok {
		return
	}
	if _, ok := a.query(w, r); !ok {
		return
	}
	j, err := a.svc.Get(r.Context(), p.UserID, id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusOK, jobBody{Job: viewJob(j, true)})
}

func (a *API) list(w http.ResponseWriter, r *http.Request, p principal) {
	q, ok := a.query(w, r, "cursor", "limit", "status")
	if !ok {
		return
	}
	limit, _, ok := a.intParam(w, r, q, "limit", 1, jobs.MaxListLimit)
	if !ok {
		return
	}
	status := q.Get("status") // "" is every job; `active` is the jobs in flight (jobs.ListStatusActive); another word is refused here and, as the second wall, by the store
	if !jobs.ValidListStatus(status) {
		a.refuseRequest(w, r, http.StatusBadRequest, codeValidationFailed, "status must be active, or left out")
		return
	}
	list, next, err := a.svc.List(r.Context(), p.UserID, jobs.ListOpts{After: q.Get("cursor"), Limit: int(limit), Status: status})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := struct {
		Jobs       []jobView `json:"jobs"`
		NextCursor *string   `json:"next_cursor"`
	}{Jobs: make([]jobView, 0, len(list)), NextCursor: strPtr(next)}
	for _, j := range list {
		out.Jobs = append(out.Jobs, viewJob(j, false))
	}
	a.writeJSON(w, r, http.StatusOK, out)
}

func (a *API) events(w http.ResponseWriter, r *http.Request, p principal) {
	id, ok := a.jobID(w, r)
	if !ok {
		return
	}
	q, ok := a.query(w, r, "after", "limit")
	if !ok {
		return
	}
	after, _, ok := a.intParam(w, r, q, "after", 0, 1<<62)
	if !ok {
		return
	}
	limit, _, ok := a.intParam(w, r, q, "limit", 1, jobs.MaxEvents)
	if !ok {
		return
	}
	evs, err := a.svc.Events(r.Context(), p.UserID, id, after, int(limit))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := struct {
		Events    []eventView `json:"events"`
		NextAfter *int64      `json:"next_after"`
	}{Events: make([]eventView, 0, len(evs))}
	for _, e := range evs {
		out.Events = append(out.Events, viewEvent(e))
	}
	// a page that is as long as the limit asked for (the store's default when none) may be followed by more: its last id is where the next page starts
	size := int(limit)
	if size == 0 {
		size = jobs.DefaultEvents
	}
	if len(evs) > 0 && len(evs) >= size {
		last := evs[len(evs)-1].ID
		out.NextAfter = &last
	}
	a.writeJSON(w, r, http.StatusOK, out)
}

// ---- results, as JSON ------------------------------------------------------------------------------------------------

// defaultResultsPage is the page a client gets when it does not say (the dial's maximum when that is smaller).
const defaultResultsPage = 100

// The cursor of the results is the ordinal of the last result of the page before, written so that a client cannot mistake it for something it may build: base64url of "o<ordinal>". It is
// opaque to a client and strict to us: anything that is not exactly what cursorOf writes is a 400.
func cursorOf(ordinal int) string {
	return base64.RawURLEncoding.EncodeToString([]byte("o" + strconv.Itoa(ordinal)))
}

func ordinalOf(cursor string) (int, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || len(raw) < 2 || raw[0] != 'o' {
		return 0, false
	}
	n, err := strconv.Atoi(string(raw[1:]))
	if err != nil || n < 1 || n > 1<<31-1 || cursorOf(n) != cursor {
		return 0, false
	}
	return n, true
}

func (a *API) results(w http.ResponseWriter, r *http.Request, p principal) {
	id, ok := a.jobID(w, r)
	if !ok {
		return
	}
	q, ok := a.query(w, r, "cursor", "limit")
	if !ok {
		return
	}
	after := 0
	if c := q.Get("cursor"); c != "" {
		if after, ok = ordinalOf(c); !ok {
			a.refuseRequest(w, r, http.StatusBadRequest, codeValidationFailed, "the cursor is not one this API gave")
			return
		}
	}
	limit, present, ok := a.intParam(w, r, q, "limit", 1, int64(a.pageMax))
	if !ok {
		return
	}
	if !present {
		limit = int64(min(defaultResultsPage, a.pageMax))
	}
	// one more than the page: its presence says that another page follows, so the last page never points at an empty one
	page, err := a.svc.Results(r.Context(), p.UserID, id, after, int(limit)+1)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := struct {
		Results    []resultView `json:"results"`
		NextCursor *string      `json:"next_cursor"`
	}{Results: make([]resultView, 0, len(page))}
	if len(page) > int(limit) {
		page = page[:limit]
		c := cursorOf(page[len(page)-1].Ordinal)
		out.NextCursor = &c
	}
	for _, res := range page {
		out.Results = append(out.Results, viewResult(res))
	}
	a.writeJSON(w, r, http.StatusOK, out)
}

// ---- the moves ------------------------------------------------------------------------------------------------------

// previewBody is the optional body of a preview: the user's choice for the `unknown` verdicts of a chained send. A preview with no body (or an empty one) is a preview of the job as it is.
type previewBody struct {
	UnknownChoice *string `json:"unknown_choice"`
}

// previewOptions reads the preview's optional body (at most 64 KiB, JSON, no member the API does not know) and returns the store's options for it. It answers the failure itself.
func (a *API) previewOptions(w http.ResponseWriter, r *http.Request) ([]jobs.PreviewOption, bool) {
	if a.tooBig(w, r, maxSmallBody) {
		return nil, false
	}
	done := extendReadDeadline(w, smallBodyWindow)
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSmallBody))
	done()
	if err != nil {
		a.badBody(w, r, err)
		return nil, false
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, true
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	var body previewBody
	if !a.readJSON(w, r, maxSmallBody, smallBodyWindow, &body) {
		return nil, false
	}
	if body.UnknownChoice == nil {
		return nil, true
	}
	return []jobs.PreviewOption{jobs.PreviewUnknownChoice(*body.UnknownChoice)}, true
}

func (a *API) preview(w http.ResponseWriter, r *http.Request, p principal) {
	id, ok := a.jobID(w, r)
	if !ok {
		return
	}
	opts, ok := a.previewOptions(w, r)
	if !ok {
		return
	}
	pv, err := a.svc.Preview(r.Context(), p.UserID, id, opts...)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	j, err := a.svc.Get(r.Context(), p.UserID, id) // the job as the preview left it (status previewed, the stored preview)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	v := viewJob(j, true)
	if v.Preview != nil {
		// the samples are in THIS answer only: they hold the recipients' data, so the store never writes them (jobs.Preview.Samples) and a later GET of the job does not have them
		v.Preview.Samples = viewSamples(pv.Samples)
	}
	a.writeJSON(w, r, http.StatusOK, jobBody{Job: v})
}

// move runs one of the store's moves that need no body and returns the job it leaves.
func (a *API) move(w http.ResponseWriter, r *http.Request, p principal, do func(ctx context.Context, userID, jobID string) (jobs.Job, error)) {
	id, ok := a.jobID(w, r)
	if !ok || !a.drain(w, r) {
		return
	}
	j, err := do(r.Context(), p.UserID, id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusOK, jobBody{Job: viewJob(j, true)})
}

func (a *API) discard(w http.ResponseWriter, r *http.Request, p principal) {
	a.move(w, r, p, a.svc.DiscardDraft)
}

func (a *API) reopen(w http.ResponseWriter, r *http.Request, p principal) {
	a.move(w, r, p, a.svc.Reopen)
}

// cancel attempts the move the job's state calls for (the store decides which, and the database whether the module can do it): a job that holds nothing at a module (confirmed, scheduled, paused
// before its start) is cancelled at once and its reserve released; a job the module holds (running, or paused while it was running) becomes cancelling and the engine ends it (the answer says
// `cancelling`, and the job ends `cancelled` when the module has reported what finished). No seeded module is refused any more; a module whose capability row says it cannot would be the typed
// refusal not_allowed / cancel_not_supported.
func (a *API) cancel(w http.ResponseWriter, r *http.Request, p principal) {
	a.move(w, r, p, a.svc.Cancel)
}

// pause holds a job: a scheduled job's start is deferred, and a RUNNING job of a module that can pause one (modules 2 and 3) is paused the moment the answer is sent (the module is told at the
// engine's next tick, and the items it had already claimed finish). The database decides: module 1 cannot, which is the typed refusal not_allowed / pause_not_supported.
func (a *API) pause(w http.ResponseWriter, r *http.Request, p principal) {
	a.move(w, r, p, a.svc.Pause)
}

// resume lets a paused job go on, to the state it paused from (scheduled or running).
func (a *API) resume(w http.ResponseWriter, r *http.Request, p principal) {
	a.move(w, r, p, a.svc.Resume)
}

// confirmBody is the body of a confirm: the fingerprint of the preview the user saw, optionally when to start, and, for a chained send whose source job holds `unknown` verdicts, the
// user's explicit choice for them (the store refuses a confirm that has none, and one that differs from the choice the job was previewed under).
type confirmBody struct {
	Fingerprint    string  `json:"fingerprint"`
	ScheduledStart *string `json:"scheduled_start"`
	UnknownChoice  *string `json:"unknown_choice"`
}

func (a *API) confirm(w http.ResponseWriter, r *http.Request, p principal) {
	id, ok := a.jobID(w, r)
	if !ok {
		return
	}
	keys := r.Header.Values("Idempotency-Key")
	switch len(keys) {
	case 0:
		a.refuseRequest(w, r, http.StatusBadRequest, codeValidationFailed, "the Idempotency-Key header is required")
		return
	case 1:
	default:
		a.refuseRequest(w, r, http.StatusBadRequest, codeValidationFailed, "send exactly one Idempotency-Key header")
		return
	}
	var body confirmBody
	if !a.readJSON(w, r, maxSmallBody, smallBodyWindow, &body) {
		return
	}
	req := jobs.ConfirmReq{JobID: id, Fingerprint: body.Fingerprint, Key: keys[0]}
	if body.UnknownChoice != nil {
		req.UnknownChoice = *body.UnknownChoice
	}
	if body.ScheduledStart != nil {
		t, err := time.Parse(time.RFC3339, *body.ScheduledStart)
		if err != nil {
			a.refuseRequest(w, r, http.StatusBadRequest, codeValidationFailed, "scheduled_start must be an RFC 3339 time, for example 2026-12-01T09:30:00Z")
			return
		}
		t = t.UTC()
		req.ScheduledStart = &t
	}
	res, err := a.svc.Confirm(r.Context(), p.UserID, req)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusOK, struct {
		Job           jobView `json:"job"`
		Replayed      bool    `json:"replayed"`
		Reserved      int64   `json:"reserved"`
		Pending       int     `json:"pending"`
		Suppressed    int     `json:"suppressed"`
		MissingFields int     `json:"missing_fields"` // a send job's rows left out, free, because a merge field they need is missing (rule P-09)
		Unrenderable  int     `json:"unrenderable"`   // a send job's rows left out, free, because their rendered message is refused (rule P-10)
	}{viewJob(res.Job, true), res.Replayed, res.Reserved, res.Pending, res.Suppressed, res.MissingFields, res.Unrenderable})
}
