package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/coldmail-platform/platform/internal/jobs"
)

// The JSON of the API. Ids and times are strings (times RFC 3339, UTC, with nanoseconds only when there are some); a time or an id that does not apply is null, never "" or a zero time. The
// views are written out here, field by field, so that nothing a store type gains is shown to a client by accident (views_test.go pins each view to the type it shows, and names what is
// left out on purpose).

// stamp formats a time as RFC 3339 in UTC.
func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func stampPtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := stamp(*t)
	return &s
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

type settingsView struct {
	UnknownChoice string `json:"unknown_choice"`
}

type contentView struct {
	Subject string `json:"subject"`
	HTML    string `json:"html"`
	Text    string `json:"text"`
	ReplyTo string `json:"reply_to"`
}

// previewView is jobs.Preview with the time as the UTC string the rest of the API uses (the embedded struct supplies every other member; the outer ComputedAt hides its time).
type previewView struct {
	jobs.Preview
	ComputedAt string `json:"computed_at"`
}

// jobView is a job as a client sees it. The owner (user_id) and the confirm key are not part of it. content is present only on a send job and is left out of a list (a list of 100 jobs must
// not carry a hundred bodies).
type jobView struct {
	ID                 string       `json:"id"`
	Module             string       `json:"module"`
	Status             string       `json:"status"`
	SourceJobID        *string      `json:"source_job_id"`
	ScheduledStart     *string      `json:"scheduled_start"`
	StartDeadline      *string      `json:"start_deadline"`
	PausedFrom         *string      `json:"paused_from"`
	Settings           settingsView `json:"settings"`
	Content            *contentView `json:"content,omitempty"`
	Preview            *previewView `json:"preview"`
	PreviewFingerprint *string      `json:"preview_fingerprint"`
	PreviewedAt        *string      `json:"previewed_at"`
	ConfirmedAt        *string      `json:"confirmed_at"`
	StartedAt          *string      `json:"started_at"`
	FinishedAt         *string      `json:"finished_at"`
	CancelRequestedAt  *string      `json:"cancel_requested_at"`
	CreatedAt          string       `json:"created_at"`
	UpdatedAt          string       `json:"updated_at"`
}

// viewJob is the job as a client sees it; withContent says whether a send job's campaign is part of it.
func viewJob(j jobs.Job, withContent bool) jobView {
	v := jobView{
		ID: j.ID, Module: j.Module, Status: j.Status,
		SourceJobID: strPtr(j.SourceJobID), ScheduledStart: stampPtr(j.ScheduledStart), StartDeadline: stampPtr(j.StartDeadline), PausedFrom: strPtr(j.PausedFrom),
		Settings:           settingsView{UnknownChoice: j.Settings.UnknownChoice},
		PreviewFingerprint: strPtr(j.PreviewFingerprint),
		PreviewedAt:        stampPtr(j.PreviewedAt), ConfirmedAt: stampPtr(j.ConfirmedAt), StartedAt: stampPtr(j.StartedAt), FinishedAt: stampPtr(j.FinishedAt),
		CancelRequestedAt: stampPtr(j.CancelRequestedAt),
		CreatedAt:         stamp(j.CreatedAt), UpdatedAt: stamp(j.UpdatedAt),
	}
	if withContent && j.Content != nil {
		v.Content = &contentView{Subject: j.Content.Subject, HTML: j.Content.HTML, Text: j.Content.Text, ReplyTo: j.Content.ReplyTo}
	}
	if j.Preview != nil {
		p := *j.Preview
		if p.MalformedReasons == nil {
			p.MalformedReasons = map[string]int{} // an object, never null: a client counts its members
		}
		v.Preview = &previewView{Preview: p, ComputedAt: stamp(p.ComputedAt)}
	}
	return v
}

type eventView struct {
	ID         int64   `json:"id"`
	Kind       string  `json:"kind"`
	FromStatus *string `json:"from_status"`
	ToStatus   *string `json:"to_status"`
	ActorKind  string  `json:"actor_kind"`
	Reason     string  `json:"reason"`
	CreatedAt  string  `json:"created_at"`
}

// viewEvent leaves out who acted (the user is the caller, an admin is not the customer's to know) and the engine's own detail.
func viewEvent(e jobs.Event) eventView {
	return eventView{ID: e.ID, Kind: e.Kind, FromStatus: strPtr(e.FromStatus), ToStatus: strPtr(e.ToStatus), ActorKind: e.ActorKind, Reason: e.Reason, CreatedAt: stamp(e.CreatedAt)}
}

type resultView struct {
	Ordinal    int     `json:"ordinal"`
	Email      string  `json:"email"`
	Origin     string  `json:"origin"`
	State      string  `json:"state"`
	Reason     *string `json:"reason"`
	Decision   string  `json:"decision"`
	Unit       *string `json:"unit"`
	Rule       string  `json:"rule"`
	RecordedAt string  `json:"recorded_at"`
}

func viewResult(r jobs.Result) resultView {
	return resultView{Ordinal: r.Ordinal, Email: r.Email, Origin: r.Origin, State: r.State, Reason: strPtr(r.Reason), Decision: r.Decision, Unit: strPtr(r.Unit), Rule: r.Rule, RecordedAt: stamp(r.RecordedAt)}
}

// writeJSON writes a success body: the status, the content type of the envelope and the JSON of v. A value that cannot be encoded is a bug and is a 500 before a byte is written.
func (a *API) writeJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}
