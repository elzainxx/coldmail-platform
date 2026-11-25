package api

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/coldmail-platform/platform/internal/csvsafe"
	"github.com/coldmail-platform/platform/internal/jobs"
)

// resultsCSVHeader is the first line of results.csv: the columns of a result, in this order, in the words of the JSON.
var resultsCSVHeader = []string{"ordinal", "email", "origin", "state", "reason", "decision", "unit", "rule"}

// resultsCSV streams every result of the job as a CSV that opens safely in a spreadsheet and reads back exactly (internal/csvsafe: every cell through encoding/csv, one leading apostrophe on a
// cell that starts with = + - @ TAB CR or '; csvsafe.Unescape is the exact inverse).
//
// MEMORY. The results are read a page at a time (jobs.ResultsPageSize, by Service.EachResult) and written through to the client: nothing but one page is held, and the response is flushed after
// each page, so a client of a 100,000-row job sees its first rows at once and the server's memory does not depend on the size of the list.
//
// A FAILURE. Before the first byte (the job is not the user's, the database is down) the answer is the ordinary error envelope. After it the status line has been sent and cannot be taken
// back, so the only honest thing left is to abort the connection (panic(http.ErrAbortHandler), which net/http turns into a connection that ends without the closing chunk): the client sees
// a body that ends in an error, never one that looks complete. The same is done when the client has gone away (its context ends): nobody is left to tell, and the walk stops at once.
func (a *API) resultsCSV(w http.ResponseWriter, r *http.Request, p principal) {
	id, ok := a.jobID(w, r)
	if !ok {
		return
	}
	if _, ok := a.query(w, r); !ok {
		return
	}
	rc := http.NewResponseController(w)
	cw := csvsafe.NewWriter(w)
	started, rows := false, 0

	start := func() error {
		h := w.Header()
		h.Set("Content-Type", "text/csv; charset=utf-8")
		h.Set("Content-Disposition", `attachment; filename="job-`+id+`.csv"`)
		h.Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusOK)
		started = true
		_ = rc.SetWriteDeadline(time.Now().Add(csvPageWindow))
		return cw.Write(resultsCSVHeader)
	}
	flush := func() error {
		cw.Flush()
		if err := cw.Error(); err != nil {
			return err
		}
		if err := rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return err
		}
		_ = rc.SetWriteDeadline(time.Now().Add(csvPageWindow)) // a client that reads each page in a minute may take as long as it needs
		return nil
	}

	err := a.svc.EachResult(r.Context(), p.UserID, id, func(res jobs.Result) error {
		if !started {
			if err := start(); err != nil {
				return err
			}
		}
		if err := cw.Write([]string{strconv.Itoa(res.Ordinal), res.Email, res.Origin, res.State, res.Reason, res.Decision, res.Unit, res.Rule}); err != nil {
			return err
		}
		rows++
		if rows%jobs.ResultsPageSize == 0 { // the end of a page: the client has it before the next page is read
			return flush()
		}
		return nil
	})
	if err == nil && !started { // a job with no result yet: the header alone is the file
		err = start()
	}
	if err == nil {
		err = flush()
	}
	if err == nil {
		return
	}
	if !started {
		a.fail(w, r, err) // nothing has been sent: the envelope
		return
	}
	if r.Context().Err() != nil {
		a.log.InfoContext(r.Context(), "results.csv stopped: the client went away", "rows_sent", rows)
	} else {
		a.log.ErrorContext(r.Context(), "results.csv aborted after it had started", "rows_sent", rows, "error", addressLike.ReplaceAllString(err.Error(), "[address]"))
	}
	panic(http.ErrAbortHandler)
}
