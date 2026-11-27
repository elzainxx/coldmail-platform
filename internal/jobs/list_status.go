package jobs

// THE STATUS FILTER OF THE LIST (Phase 6 A3: GET /v1/jobs?status=active). ListOpts.Status is "" (every job of the user, as before) or ListStatusActive. The filter is part of the SQL, not of the page
// that was read: a page is the next jobs that match, in the same order and with the same cursor, so a cursor taken from a filtered page continues the filtered list.

// ListStatusActive is the filter of the jobs that are in flight and that a user watches or acts on: scheduled (waiting for its start), running, paused and cancelling. A job that is still confirmed or
// completing is between two steps of the engine and is not listed here; a draft, a previewed job and the finished ones (succeeded, failed, cancelled, expired) are not active.
const ListStatusActive = "active"

// activeListStatuses are the statuses ListStatusActive keeps.
var activeListStatuses = []string{StatusScheduled, StatusRunning, StatusPaused, StatusCancelling}

// ActiveListStatuses are the statuses ListStatusActive keeps, as a copy (the documentation and the tests read them; nothing can change the filter through the slice).
func ActiveListStatuses() []string { return append([]string(nil), activeListStatuses...) }

// ValidListStatus says whether a value of ListOpts.Status is one the store knows: empty (no filter) or ListStatusActive.
func ValidListStatus(s string) bool { return s == "" || s == ListStatusActive }

// listStatusFilter is the statuses a list keeps for the filter, nil for none, or a bad request for a value the store does not know.
func listStatusFilter(s string) ([]string, error) {
	switch s {
	case "":
		return nil, nil
	case ListStatusActive:
		return activeListStatuses, nil
	}
	return nil, badRequest("status must be %q or left out", ListStatusActive)
}
