package m1

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/coldmail-platform/platform/internal/modclient"
)

// JobCounts is GET /v1/jobs/{id}'s counts over input rows: Pending = Total - Settled.
type JobCounts struct {
	Total   int
	Settled int
	Pending int
}

// StatusCounts is a count per verdict status, the four of module 1's closed vocabulary, zero included.
type StatusCounts struct {
	Valid   int
	Invalid int
	Risky   int
	Unknown int
}

// Sum is the total of the four.
func (c StatusCounts) Sum() int { return c.Valid + c.Invalid + c.Risky + c.Unknown }

// JobTotals is GET /v1/jobs/{id}'s `totals` (Phase 3 close-out): the numbers of the job's INPUT ROWS, read by the module in the same snapshot as the counts and `complete`.
// Submitted = Settled + Pending, and ByStatus.Sum() = Settled; Status refuses an answer in which either does not hold.
type JobTotals struct {
	// Submitted is the number of input rows the job recorded (malformed and duplicate rows included): the 202's counts.total.
	Submitted int
	// Settled is the rows that have an outcome: the number of rows the results listing serves at that instant.
	Settled int
	// Pending is Submitted - Settled.
	Pending int
	// Duplicates is the rows that repeat an earlier row's normalised address (the 202's counts.duplicate); Malformed the rows that did not normalise (the 202's counts.malformed).
	Duplicates int
	Malformed  int
	// ByStatus counts the SETTLED rows by verdict status (a malformed row is invalid; a duplicate row counts under its first row's verdict).
	ByStatus StatusCounts
}

// JobStatus is GET /v1/jobs/{id}. Complete is module 1's own judgement, decided from its work items and never from the counts: branch on it, not on Status (M1-F13).
type JobStatus struct {
	JobID            string
	Status           string
	Complete         bool
	Counts           JobCounts
	Totals           JobTotals
	ProcessingPaused bool
}

// validJobID says whether id is a uuid in the 8-4-4-4-12 hexadecimal form, the only shape module 1's job routes take.
func validJobID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i := 0; i < 36; i++ {
		c := id[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
				return false
			}
		}
	}
	return true
}

// checkJobID refuses an id that is not a uuid before any request. This is plain input validation: module 1 answers a job route called with such an id by 400 validation_failed
// (details field `id`, issue `malformed`, since the Phase 3 close-out; it used to be a 500 internal_error), and that answer, if it ever comes (an id the module judges stricter than
// this check), reaches the caller as a Refused error with its code and details (TestJobRoutes_AMalformedIDTheModuleRefusesIsRefusedNotInternal).
func checkJobID(route, id string) *modclient.Error {
	if !validJobID(id) {
		return newErr(modclient.Invalid, route, "the job id is not a uuid, so no request was made")
	}
	return nil
}

// Status reads the job's status, counts and totals: GET /v1/jobs/{id}, idempotent, retried per the dials. The status word is a closed vocabulary: a word outside it, or `complete: true`
// beside a word that is not a finished one, is a Contract error naming the key, never "still running". So is a count that is negative and a set of numbers that contradict each
// other (they come from one snapshot, so they cannot honestly): totals.submitted is not settled + pending, the four by_status counts do not add up to settled, counts.total is not
// totals.submitted, counts.settled is not totals.settled, or counts.pending is not totals.pending. ON ANY ERROR THE RESULT IS THE ZERO JobStatus.
func (a *Adapter) Status(ctx context.Context, id string) (JobStatus, error) {
	if e := checkJobID(routeStatus, id); e != nil {
		return JobStatus{}, e
	}
	resp, err := a.c.Do(ctx, modclient.Request{
		Module: modclient.M1, Route: routeStatus, Method: http.MethodGet, Path: "/v1/jobs/" + url.PathEscape(id), Idempotent: true,
	})
	if err != nil {
		return JobStatus{}, err
	}
	var w struct {
		JobID    string `json:"job_id"`
		Status   string `json:"status"`
		Complete bool   `json:"complete"`
		Counts   struct {
			Total   int `json:"total"`
			Settled int `json:"settled"`
			Pending int `json:"pending"`
		} `json:"counts"`
		Totals struct {
			Submitted  int `json:"submitted"`
			Settled    int `json:"settled"`
			Pending    int `json:"pending"`
			Duplicates int `json:"duplicates"`
			Malformed  int `json:"malformed"`
			ByStatus   struct {
				Valid   int `json:"valid"`
				Invalid int `json:"invalid"`
				Risky   int `json:"risky"`
				Unknown int `json:"unknown"`
			} `json:"by_status"`
		} `json:"totals"`
		ProcessingPaused bool `json:"processing_paused"`
	}
	if err := modclient.Decode(resp, &w, jobShape); err != nil {
		return JobStatus{}, err
	}
	// MODULE-FORCED(M1-F13): module 1's status word is the stored one passed through, and its database vocabulary has four words of which the code writes two (running, and complete
	// derived at read time); the only authoritative answer to "is it finished" is the boolean, so the adapter accepts exactly the four words, requires the boolean and the word to agree
	// when the boolean says finished, and never reads the word as progress.
	t := w.Totals
	by := StatusCounts{Valid: t.ByStatus.Valid, Invalid: t.ByStatus.Invalid, Risky: t.ByStatus.Risky, Unknown: t.ByStatus.Unknown}
	switch {
	case !strings.EqualFold(w.JobID, id):
		return JobStatus{}, contractErr(routeStatus, resp, "job_id", "the answer is about another job")
	case !knownJobStatus(w.Status):
		return JobStatus{}, contractErr(routeStatus, resp, "status", "the job status is not one of module 1's four words")
	case w.Complete && !completeJobStatus(w.Status):
		return JobStatus{}, contractErr(routeStatus, resp, "status", "the job says complete: true beside a status that is not a finished one")
	case w.Counts.Total < 0:
		return JobStatus{}, contractErr(routeStatus, resp, "counts.total", "a count is negative")
	case w.Counts.Settled < 0:
		return JobStatus{}, contractErr(routeStatus, resp, "counts.settled", "a count is negative")
	case w.Counts.Pending < 0:
		return JobStatus{}, contractErr(routeStatus, resp, "counts.pending", "a count is negative")
	case t.Submitted < 0:
		return JobStatus{}, contractErr(routeStatus, resp, "totals.submitted", "a count is negative")
	case t.Settled < 0:
		return JobStatus{}, contractErr(routeStatus, resp, "totals.settled", "a count is negative")
	case t.Pending < 0:
		return JobStatus{}, contractErr(routeStatus, resp, "totals.pending", "a count is negative")
	case t.Duplicates < 0:
		return JobStatus{}, contractErr(routeStatus, resp, "totals.duplicates", "a count is negative")
	case t.Malformed < 0:
		return JobStatus{}, contractErr(routeStatus, resp, "totals.malformed", "a count is negative")
	case by.Valid < 0:
		return JobStatus{}, contractErr(routeStatus, resp, "totals.by_status.valid", "a count is negative")
	case by.Invalid < 0:
		return JobStatus{}, contractErr(routeStatus, resp, "totals.by_status.invalid", "a count is negative")
	case by.Risky < 0:
		return JobStatus{}, contractErr(routeStatus, resp, "totals.by_status.risky", "a count is negative")
	case by.Unknown < 0:
		return JobStatus{}, contractErr(routeStatus, resp, "totals.by_status.unknown", "a count is negative")
	// the identities of one snapshot: the module serves the counts, the totals and `complete` from ONE read, and refuses to serve numbers that break these, so an answer that does is the
	// module contradicting itself and nobody can tell which half to believe
	case t.Submitted != t.Settled+t.Pending:
		return JobStatus{}, contractErr(routeStatus, resp, "totals.submitted", "the totals do not add up (submitted is not settled + pending)")
	case by.Sum() != t.Settled:
		return JobStatus{}, contractErr(routeStatus, resp, "totals.by_status", "the totals do not add up (the four statuses do not sum to settled)")
	case w.Counts.Total != t.Submitted:
		return JobStatus{}, contractErr(routeStatus, resp, "counts.total", "the counts and the totals of one answer disagree (counts.total is not totals.submitted)")
	case w.Counts.Settled != t.Settled:
		return JobStatus{}, contractErr(routeStatus, resp, "counts.settled", "the counts and the totals of one answer disagree (counts.settled is not totals.settled)")
	case w.Counts.Pending != t.Pending:
		return JobStatus{}, contractErr(routeStatus, resp, "counts.pending", "the counts and the totals of one answer disagree (counts.pending is not totals.pending)")
	}
	return JobStatus{
		JobID: id, Status: w.Status, Complete: w.Complete, ProcessingPaused: w.ProcessingPaused,
		Counts: JobCounts{Total: w.Counts.Total, Settled: w.Counts.Settled, Pending: w.Counts.Pending},
		Totals: JobTotals{Submitted: t.Submitted, Settled: t.Settled, Pending: t.Pending, Duplicates: t.Duplicates, Malformed: t.Malformed, ByStatus: by},
	}, nil
}
