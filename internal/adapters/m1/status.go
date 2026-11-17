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

// JobStatus is GET /v1/jobs/{id}. Complete is module 1's own judgement, decided from its work items and never from the counts: branch on it, not on Status (M1-F13).
type JobStatus struct {
	JobID            string
	Status           string
	Complete         bool
	Counts           JobCounts
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

// checkJobID refuses an id that is not a uuid before any request.
//
// MODULE-FORCED(M1-F1): module 1's jobs.id column is a uuid, so a job route called with anything else is a store ERROR and answers 500 internal_error, not 404 (research m1 1.8
// item 3; pg_tenancy_test.go says so too): a 500 would be retried as a module fault and could not be told from a real one, so the adapter validates the shape and refuses locally.
func checkJobID(route, id string) *modclient.Error {
	if !validJobID(id) {
		return newErr(modclient.Invalid, route, "the job id is not a uuid (module 1 answers a job route with a non-uuid id by 500, so none is sent)")
	}
	return nil
}

// Status reads the job's status and counts: GET /v1/jobs/{id}, idempotent, retried per the dials. The status word is a closed vocabulary: a word outside it, or `complete: true`
// beside a word that is not a finished one, is a Contract error naming the key, never "still running".
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
		ProcessingPaused bool `json:"processing_paused"`
	}
	if err := modclient.Decode(resp, &w, jobShape); err != nil {
		return JobStatus{}, err
	}
	// MODULE-FORCED(M1-F13): module 1's status word is the stored one passed through, and its database vocabulary has four words of which the code writes two (running, and complete
	// derived at read time); the only authoritative answer to "is it finished" is the boolean, so the adapter accepts exactly the four words, requires the boolean and the word to agree
	// when the boolean says finished, and never reads the word as progress.
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
	}
	return JobStatus{
		JobID: id, Status: w.Status, Complete: w.Complete, ProcessingPaused: w.ProcessingPaused,
		Counts: JobCounts{Total: w.Counts.Total, Settled: w.Counts.Settled, Pending: w.Counts.Pending},
	}, nil
}
