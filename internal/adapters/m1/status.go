package m1

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/coldmail-platform/platform/internal/modclient"
)

// JobCounts is GET /v1/jobs/{id}'s counts over input rows: Pending = Total - Settled - Cancelled.
type JobCounts struct {
	Total   int
	Settled int
	Pending int
	// Cancelled is the rows cancelled before their check began (Phase 4 close-out C2, module 1 migration 0020): 0 for a module that has no cancel and for a job never cancelled (the key is
	// absent from an older module's answer and reads as 0).
	Cancelled int
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
// Submitted = Settled + Cancelled + Pending, and ByStatus.Sum() = Settled (a cancelled row carries no verdict); Status refuses an answer in which either does not hold.
type JobTotals struct {
	// Submitted is the number of input rows the job recorded (malformed and duplicate rows included): the 202's counts.total.
	Submitted int
	// Settled is the rows that have a VERDICT: the number of verdict rows the results listing serves at that instant.
	Settled int
	// Cancelled is the rows cancelled before their check began: free, no verdict, served by the listing with status `cancelled`. 0 when the key is absent (an older module).
	Cancelled int
	// Pending is Submitted - Settled - Cancelled: the rows still to be decided.
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
	// CancelRequested is module 1's `cancel_requested`: a cancel of this job was accepted (POST /v1/jobs/{id}/cancel). false when the key is absent (an older module) and for a job whose
	// cancel found it already complete (nothing was cancelled, so it is never marked).
	CancelRequested bool
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
// other (they come from one snapshot, so they cannot honestly): totals.submitted is not settled + cancelled + pending (cancelled is 0 and absent from an older module's answer), the
// four by_status counts do not add up to settled, counts.total is not totals.submitted, counts.settled is not totals.settled, counts.pending is not totals.pending, or counts.cancelled is
// not totals.cancelled. ON ANY ERROR THE RESULT IS THE ZERO JobStatus.
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
	var w wireStatus
	if err := modclient.Decode(resp, &w, jobShape); err != nil {
		return JobStatus{}, err
	}
	return judgeStatus(routeStatus, resp, id, w)
}

// wireStatus is the job-status body as it is decoded (after jobShape, or cancelShape for the cancel route, accepted it): the keys Status keeps. counts.cancelled, totals.cancelled and
// cancel_requested are ADDITIVE (module 1 migration 0020, owner ruling C2): an older module's answer has none of them and they read 0 and false.
type wireStatus struct {
	JobID    string `json:"job_id"`
	Status   string `json:"status"`
	Complete bool   `json:"complete"`
	Counts   struct {
		Total     int `json:"total"`
		Settled   int `json:"settled"`
		Pending   int `json:"pending"`
		Cancelled int `json:"cancelled"`
	} `json:"counts"`
	Totals struct {
		Submitted  int `json:"submitted"`
		Settled    int `json:"settled"`
		Pending    int `json:"pending"`
		Duplicates int `json:"duplicates"`
		Malformed  int `json:"malformed"`
		Cancelled  int `json:"cancelled"`
		ByStatus   struct {
			Valid   int `json:"valid"`
			Invalid int `json:"invalid"`
			Risky   int `json:"risky"`
			Unknown int `json:"unknown"`
		} `json:"by_status"`
	} `json:"totals"`
	ProcessingPaused bool `json:"processing_paused"`
	CancelRequested  bool `json:"cancel_requested"`
}

// judgeStatus is the ONE validation of a job-status body, shared by Status (GET /v1/jobs/{id}) and Cancel (POST /v1/jobs/{id}/cancel, whose answer is the same body plus cancel_outcome): route
// is the label an error names. On any refusal the result is the zero JobStatus.
func judgeStatus(route string, resp *modclient.Response, id string, w wireStatus) (JobStatus, error) {
	// MODULE-FORCED(M1-F13): module 1's status word is the stored one passed through, and its database vocabulary has four words of which the code writes two (running, and complete
	// derived at read time); the only authoritative answer to "is it finished" is the boolean, so the adapter accepts exactly the four words, requires the boolean and the word to agree
	// when the boolean says finished, and never reads the word as progress.
	t := w.Totals
	by := StatusCounts{Valid: t.ByStatus.Valid, Invalid: t.ByStatus.Invalid, Risky: t.ByStatus.Risky, Unknown: t.ByStatus.Unknown}
	switch {
	case !strings.EqualFold(w.JobID, id):
		return JobStatus{}, contractErr(route, resp, "job_id", "the answer is about another job")
	case !knownJobStatus(w.Status):
		return JobStatus{}, contractErr(route, resp, "status", "the job status is not one of module 1's four words")
	case w.Complete && !completeJobStatus(w.Status):
		return JobStatus{}, contractErr(route, resp, "status", "the job says complete: true beside a status that is not a finished one")
	case w.Counts.Total < 0:
		return JobStatus{}, contractErr(route, resp, "counts.total", "a count is negative")
	case w.Counts.Settled < 0:
		return JobStatus{}, contractErr(route, resp, "counts.settled", "a count is negative")
	case w.Counts.Pending < 0:
		return JobStatus{}, contractErr(route, resp, "counts.pending", "a count is negative")
	case w.Counts.Cancelled < 0:
		return JobStatus{}, contractErr(route, resp, "counts.cancelled", "a count is negative")
	case t.Submitted < 0:
		return JobStatus{}, contractErr(route, resp, "totals.submitted", "a count is negative")
	case t.Settled < 0:
		return JobStatus{}, contractErr(route, resp, "totals.settled", "a count is negative")
	case t.Pending < 0:
		return JobStatus{}, contractErr(route, resp, "totals.pending", "a count is negative")
	case t.Cancelled < 0:
		return JobStatus{}, contractErr(route, resp, "totals.cancelled", "a count is negative")
	case t.Duplicates < 0:
		return JobStatus{}, contractErr(route, resp, "totals.duplicates", "a count is negative")
	case t.Malformed < 0:
		return JobStatus{}, contractErr(route, resp, "totals.malformed", "a count is negative")
	case by.Valid < 0:
		return JobStatus{}, contractErr(route, resp, "totals.by_status.valid", "a count is negative")
	case by.Invalid < 0:
		return JobStatus{}, contractErr(route, resp, "totals.by_status.invalid", "a count is negative")
	case by.Risky < 0:
		return JobStatus{}, contractErr(route, resp, "totals.by_status.risky", "a count is negative")
	case by.Unknown < 0:
		return JobStatus{}, contractErr(route, resp, "totals.by_status.unknown", "a count is negative")
	// the identities of one snapshot: the module serves the counts, the totals and `complete` from ONE read, and refuses to serve numbers that break these, so an answer that does is the
	// module contradicting itself and nobody can tell which half to believe. Since module 1's cancel (migration 0020) every input row is settled (has a verdict), cancelled (final, free,
	// no verdict) or pending, and a job is complete when every row is final, cancelled included.
	case t.Submitted != t.Settled+t.Cancelled+t.Pending:
		return JobStatus{}, contractErr(route, resp, "totals.submitted", "the totals do not add up (submitted is not settled + cancelled + pending)")
	case by.Sum() != t.Settled:
		return JobStatus{}, contractErr(route, resp, "totals.by_status", "the totals do not add up (the four statuses do not sum to settled: a cancelled row has no verdict)")
	case w.Counts.Total != t.Submitted:
		return JobStatus{}, contractErr(route, resp, "counts.total", "the counts and the totals of one answer disagree (counts.total is not totals.submitted)")
	case w.Counts.Settled != t.Settled:
		return JobStatus{}, contractErr(route, resp, "counts.settled", "the counts and the totals of one answer disagree (counts.settled is not totals.settled)")
	case w.Counts.Pending != t.Pending:
		return JobStatus{}, contractErr(route, resp, "counts.pending", "the counts and the totals of one answer disagree (counts.pending is not totals.pending)")
	case w.Counts.Cancelled != t.Cancelled:
		return JobStatus{}, contractErr(route, resp, "counts.cancelled", "the counts and the totals of one answer disagree (counts.cancelled is not totals.cancelled)")
	}
	return JobStatus{
		JobID: id, Status: w.Status, Complete: w.Complete, ProcessingPaused: w.ProcessingPaused, CancelRequested: w.CancelRequested,
		Counts: JobCounts{Total: w.Counts.Total, Settled: w.Counts.Settled, Pending: w.Counts.Pending, Cancelled: w.Counts.Cancelled},
		Totals: JobTotals{Submitted: t.Submitted, Settled: t.Settled, Cancelled: t.Cancelled, Pending: t.Pending, Duplicates: t.Duplicates, Malformed: t.Malformed, ByStatus: by},
	}, nil
}
