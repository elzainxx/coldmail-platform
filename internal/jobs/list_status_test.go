package jobs

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// The status filter of the list (Phase 6 A3, list_status.go). Its SQL is proved against a real database by tests/harness (TestJobsAPI_ListStatusActive...); what needs no database is proved here: which
// statuses "active" means, that nothing else is accepted and that the filter cannot be changed through the slice it was read from.

func TestListStatus_ActiveIsExactlyTheFourStatusesTheSpecNames(t *testing.T) {
	want := []string{StatusScheduled, StatusRunning, StatusPaused, StatusCancelling}
	if got := ActiveListStatuses(); !reflect.DeepEqual(got, want) {
		t.Fatalf("active = %v, want %v", got, want)
	}
	got, err := listStatusFilter(ListStatusActive)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("listStatusFilter(active) = %v, %v", got, err)
	}
	// none of the others is active: the drafts and the previews hold nothing, the two transient states are between two steps of the engine, the finished ones are finished
	all := []string{StatusDraft, StatusPreviewed, StatusConfirmed, StatusScheduled, StatusRunning, StatusPaused, StatusCancelling, StatusCompleting, StatusSucceeded, StatusFailed, StatusCancelled, StatusExpired}
	in := map[string]bool{}
	for _, s := range got {
		in[s] = true
	}
	for _, s := range all {
		isWanted := s == StatusScheduled || s == StatusRunning || s == StatusPaused || s == StatusCancelling
		if in[s] != isWanted {
			t.Errorf("status %s: kept by the filter = %v, want %v", s, in[s], isWanted)
		}
	}
	if len(all) != 12 {
		t.Fatalf("the lifecycle has %d statuses listed here, want 12: a status was added or removed, and this table must say whether it is active", len(all))
	}
}

func TestListStatus_NothingButEmptyAndActiveIsAFilterAndEmptyIsNone(t *testing.T) {
	if got, err := listStatusFilter(""); err != nil || got != nil {
		t.Fatalf("no filter = %v, %v; want nil and no error (a NULL parameter keeps every status)", got, err)
	}
	for _, bad := range []string{"ACTIVE", " active", "active ", "running", "all", "finished", "draft", "active,paused", "%", "'; DROP TABLE platform.jobs; --"} {
		got, err := listStatusFilter(bad)
		var br *BadRequestError
		if got != nil || !errors.As(err, &br) || !errors.Is(err, ErrBadRequest) {
			t.Errorf("listStatusFilter(%q) = %v, %v; want a bad request", bad, got, err)
			continue
		}
		if strings.Contains(br.Reason, "DROP") || strings.Contains(br.Reason, "finished") {
			t.Errorf("the reason quotes the value: %q", br.Reason)
		}
		if ValidListStatus(bad) {
			t.Errorf("ValidListStatus(%q) = true", bad)
		}
	}
	if !ValidListStatus("") || !ValidListStatus(ListStatusActive) {
		t.Fatal("the two values of the filter are not valid")
	}
}

func TestListStatus_TheFilterCannotBeChangedThroughTheSliceItWasReadFrom(t *testing.T) {
	a := ActiveListStatuses()
	a[0] = "draft"
	if b := ActiveListStatuses(); b[0] != StatusScheduled {
		t.Fatalf("a caller changed the filter through the copy: %v", b)
	}
	// and the twin: the slice the store itself uses is the package's own, unchanged
	if activeListStatuses[0] != StatusScheduled {
		t.Fatal("the package's filter was changed")
	}
}
