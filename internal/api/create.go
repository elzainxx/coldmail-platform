package api

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/coldmail-platform/platform/internal/jobs"
)

// POST /v1/jobs makes a draft from a list, in one of two shapes chosen by the Content-Type:
//
//	application/json  {"module","rows":[{"email","fields"}],"content":{"subject","html","text","reply_to"},"source_job_id","settings":{"unknown_choice"}}
//	text/csv          the list is the body (a header row, the column `email` is the address, every other column is a merge field); the rest is in the query:
//	                  module, source_job_id, unknown_choice, subject, html, text, reply_to (percent-encoded, 64 KiB of query in all)
//
// Both end in the store's CreateDraft, which judges the request (the module, the content, the rows, the limit); this file only reads the bytes into a jobs.CreateReq.

type createBody struct {
	Module      string        `json:"module"`
	Rows        []rowBody     `json:"rows"`
	Content     *contentBody  `json:"content"`
	SourceJobID string        `json:"source_job_id"`
	Settings    *settingsBody `json:"settings"`
}

type rowBody struct {
	Email  string            `json:"email"`
	Fields map[string]string `json:"fields"`
}

type contentBody struct {
	Subject string `json:"subject"`
	HTML    string `json:"html"`
	Text    string `json:"text"`
	ReplyTo string `json:"reply_to"`
}

type settingsBody struct {
	UnknownChoice string `json:"unknown_choice"`
}

func (a *API) create(w http.ResponseWriter, r *http.Request, p principal) {
	var req jobs.CreateReq
	switch mediaType(r) {
	case "text/csv":
		var ok bool
		if req, ok = a.createFromCSV(w, r); !ok {
			return
		}
	case "application/json":
		var body createBody
		if !a.readJSON(w, r, maxCreateBody, createBodyWindow, &body) {
			return
		}
		req = jobs.CreateReq{Module: body.Module, SourceJobID: body.SourceJobID}
		if body.Content != nil {
			req.Content = &jobs.Content{Subject: body.Content.Subject, HTML: body.Content.HTML, Text: body.Content.Text, ReplyTo: body.Content.ReplyTo}
		}
		if body.Settings != nil {
			req.Settings = jobs.Settings{UnknownChoice: body.Settings.UnknownChoice}
		}
		if len(body.Rows) > 0 {
			req.Rows = make([]jobs.RowIn, len(body.Rows))
			for i, row := range body.Rows {
				req.Rows[i] = jobs.RowIn{Email: row.Email, Fields: row.Fields}
			}
		}
	default:
		a.refuseRequest(w, r, http.StatusUnsupportedMediaType, codeUnsupportedMedia, "send the list as application/json or text/csv")
		return
	}
	j, err := a.svc.CreateDraft(r.Context(), p.UserID, req)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	w.Header().Set("Location", "/v1/jobs/"+j.ID)
	a.writeJSON(w, r, http.StatusCreated, jobBody{Job: viewJob(j, true)})
}

// csvQueryParams are the parameters of a CSV create. Content is present when any of its four is. email_column (Phase 6 A3) names the header of the column that is the address; absent or empty, it is `email`.
var csvQueryParams = []string{"module", "source_job_id", "unknown_choice", "subject", "html", "text", "reply_to", "email_column"}

// createFromCSV reads a CSV create: the campaign and the options from the query, the list from the body. It answers the failure itself.
func (a *API) createFromCSV(w http.ResponseWriter, r *http.Request) (jobs.CreateReq, bool) {
	if len(r.URL.RawQuery) > maxCSVQuery {
		a.fail(w, r, &http.MaxBytesError{Limit: maxCSVQuery})
		return jobs.CreateReq{}, false
	}
	q, ok := a.query(w, r, csvQueryParams...)
	if !ok || a.tooBig(w, r, maxCreateBody) {
		return jobs.CreateReq{}, false
	}
	req := jobs.CreateReq{Module: q.Get("module"), SourceJobID: q.Get("source_job_id"), Settings: jobs.Settings{UnknownChoice: q.Get("unknown_choice")}}
	for _, name := range []string{"subject", "html", "text", "reply_to"} {
		if q.Has(name) {
			req.Content = &jobs.Content{Subject: q.Get("subject"), HTML: q.Get("html"), Text: q.Get("text"), ReplyTo: q.Get("reply_to")}
			break
		}
	}
	emailColumn := q.Get("email_column")
	if emailColumn != "" && strings.TrimSpace(emailColumn) == "" { // present and blank: not the same as absent, which is `email`
		a.refuseRequest(w, r, http.StatusBadRequest, codeValidationFailed, "email_column must name a column of the header")
		return jobs.CreateReq{}, false
	}
	defer extendReadDeadline(w, createBodyWindow)()
	rows, err := readCSVRows(http.MaxBytesReader(w, r.Body, maxCreateBody), a.svc.MaxRows(), req.SourceJobID != "", emailColumn)
	if err != nil {
		a.csvFailed(w, r, err)
		return jobs.CreateReq{}, false
	}
	req.Rows = rows
	return req, true
}

// csvFailed answers a CSV body that could not be read: over the byte cap is 413, over the row cap is 413 too_many_rows, anything else a 400 `validation_failed` that names the line, never
// the content of it.
func (a *API) csvFailed(w http.ResponseWriter, r *http.Request, err error) {
	var many *jobs.TooManyRowsError
	var big *http.MaxBytesError
	var bad *csvError
	switch {
	case errors.As(err, &big), errors.As(err, &many):
		a.fail(w, r, err)
	case errors.As(err, &bad):
		a.refuseRequest(w, r, http.StatusBadRequest, codeValidationFailed, bad.reason)
	default:
		a.refuseRequest(w, r, http.StatusBadRequest, codeBadRequest, "the body could not be read")
	}
}

// csvError is a CSV the platform refuses, with the reason in the API's words (a line number, a count, a column's position: never a cell).
type csvError struct{ reason string }

func (e *csvError) Error() string { return "csv: " + e.reason }

func csvRefused(reason string) error { return &csvError{reason: reason} }

// readCSVRows reads the list: the first record is the header and must name the address column exactly once (any letter case, surrounding spaces ignored): `email`, or the header emailColumn names
// when the create gave one (the query parameter email_column, Phase 6 A3); every other column becomes the merge field of its name. A record with another number of columns than the header is refused
// with its line. At most maxRows rows are read: the next one is a *jobs.TooManyRowsError and the rest of the body is left unread. An empty body is an empty list only for a chained send (empty is
// true), which takes its rows from the source job.
//
// WITH AN EXPLICIT ADDRESS COLUMN (one that is not `email`) THE TWO RESERVED NAMES ARE REFUSED AS MERGE COLUMNS. A column named `email` or `unsubscribe_url` (any letter case) that is not the address
// would become a merge field of that name, and the renderer always supplies those two itself (the row's address and its unsubscribe link): the column's values would be dead data, silently shadowed in
// every template that names them, and `{{email}}` would show the address column's value where the author meant the other column. The create is refused instead, in words that name no value, and the
// author renames the column. With `email` as the address (the default, or email_column=email) nothing changes: a column named `unsubscribe_url` is accepted and shadowed as it always was.
func readCSVRows(body io.Reader, maxRows int, empty bool, emailColumn string) ([]jobs.RowIn, error) {
	want := strings.TrimSpace(emailColumn)
	explicit := want != "" && !strings.EqualFold(want, "email")
	if want == "" {
		want = "email"
	}
	br := bufio.NewReader(body)
	if head, err := br.Peek(3); err == nil && bytes.Equal(head, []byte{0xEF, 0xBB, 0xBF}) {
		_, _ = br.Discard(3) // a byte order mark, which a spreadsheet writes before the header
	}
	cr := csv.NewReader(br)
	cr.ReuseRecord = false
	header, err := cr.Read()
	if errors.Is(err, io.EOF) {
		if empty {
			return nil, nil
		}
		return nil, csvRefused("the CSV has no header row")
	}
	if err != nil {
		return nil, csvReadError(err)
	}
	emailCol := -1
	names := make([]string, len(header))
	seen := map[string]bool{}
	for i, h := range header {
		name := strings.TrimSpace(h)
		switch {
		case strings.EqualFold(name, want):
			if emailCol >= 0 {
				if explicit {
					return nil, csvRefused("the header names the email_column column twice")
				}
				return nil, csvRefused("the header names the email column twice")
			}
			emailCol = i
			continue
		case explicit && strings.EqualFold(name, "email"):
			return nil, csvRefused("the header has a column named email that is not the address column: email is reserved for the address, so rename that column")
		case explicit && strings.EqualFold(name, "unsubscribe_url"):
			return nil, csvRefused("the header has a column named unsubscribe_url: that name is reserved for the unsubscribe link, so rename that column")
		case name == "":
			return nil, csvRefused("column " + strconv.Itoa(i+1) + " of the header has no name")
		case seen[name]:
			return nil, csvRefused("the header names a column twice")
		}
		seen[name] = true
		names[i] = name
	}
	if emailCol < 0 {
		if explicit {
			return nil, csvRefused("the header has no column named by email_column")
		}
		return nil, csvRefused("the header has no email column")
	}
	var rows []jobs.RowIn
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			return rows, nil
		}
		if err != nil {
			return nil, csvReadError(err)
		}
		if len(rows) >= maxRows {
			return nil, &jobs.TooManyRowsError{Limit: maxRows}
		}
		row := jobs.RowIn{Email: rec[emailCol]}
		if len(header) > 1 {
			row.Fields = make(map[string]string, len(header)-1)
			for i, v := range rec {
				if i != emailCol {
					row.Fields[names[i]] = v
				}
			}
		}
		rows = append(rows, row)
	}
}

// csvReadError turns the reader's error into a refusal that names the line and what was wrong in the API's words, or passes on a failure of the body itself (the byte cap, a dropped connection).
func csvReadError(err error) error {
	var pe *csv.ParseError
	if !errors.As(err, &pe) {
		return err
	}
	line := strconv.Itoa(pe.StartLine)
	switch {
	case errors.Is(err, csv.ErrFieldCount):
		return csvRefused("line " + line + " has a different number of columns than the header")
	case errors.Is(err, csv.ErrBareQuote):
		return csvRefused("line " + line + " has a quote inside an unquoted field")
	case errors.Is(err, csv.ErrQuote):
		return csvRefused("line " + line + " has a quoted field that is not closed properly")
	}
	return csvRefused("line " + line + " is not valid CSV")
}
